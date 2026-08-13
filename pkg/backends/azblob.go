package backends

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
)

// AzBlobConfig holds Azure-specific configuration for the Azure Blob Storage backend.
// These are resolved from environment variables in main.go using the
// GOBUILDCACHE_-prefixed convention (see resolveAzBlobConfig), allowing users to
// provide Azure credentials without polluting the environment for other processes
// (e.g., test binaries spawned by go test).
type AzBlobConfig struct {
	// Account is the storage account name, used to build the service endpoint
	// (https://<account>.blob.core.windows.net). When set, account-based auth
	// (DefaultAzureCredential, or SASToken if provided) is used in preference to
	// any ConnectionString, matching Microsoft's recommended order.
	Account string
	// ConnectionString typically embeds the storage account key (Shared Key),
	// which Microsoft recommends disallowing. It is the least-preferred method
	// and is used only when Account is not set; prefer DefaultAzureCredential or
	// a SAS token.
	ConnectionString string
	// SASToken, if set (together with Account), authenticates via a shared
	// access signature. A user delegation SAS (signed with Entra credentials
	// rather than the account key) is recommended. A leading "?" is optional.
	SASToken string
}

// AzBlob implements Backend using Azure Blob Storage.
// This backend only handles Azure operations; local disk caching is handled by server.go.
type AzBlob struct {
	client    *azblob.Client
	container string
	prefix    string
	ctx       context.Context
}

// NewAzBlob creates a new Azure Blob Storage-based cache backend.
// containerName is the Azure container (equivalent to an S3/GCS bucket) where cache
// files will be stored. prefix is an optional prefix for all blob names.
//
// Authentication follows Microsoft's recommended order of preference:
//   - cfg.Account set, no SAS: DefaultAzureCredential (managed identity, az
//     login, env vars, etc.) — Microsoft's recommended approach.
//   - cfg.Account set with cfg.SASToken: shared access signature (a user
//     delegation SAS is preferred).
//   - cfg.ConnectionString (Shared Key): least preferred, used only when
//     cfg.Account is not set.
func NewAzBlob(containerName, prefix string, cfg AzBlobConfig) (*AzBlob, error) {
	ctx := context.Background()

	var client *azblob.Client
	var err error
	switch {
	case cfg.Account != "":
		// Account-based auth (the Entra family) is preferred over a connection
		// string, matching Microsoft's recommended order (Entra ID / SAS over
		// Shared Key). Within the family, DefaultAzureCredential (Entra ID /
		// managed identity) is the default — Microsoft's top recommendation —
		// and a SAS is used only when a token is explicitly provided.
		if cfg.SASToken != "" {
			sas := strings.TrimPrefix(cfg.SASToken, "?")
			url := fmt.Sprintf("https://%s.blob.core.windows.net/?%s", cfg.Account, sas)
			client, err = azblob.NewClientWithNoCredential(url, nil)
		} else {
			cred, cerr := azidentity.NewDefaultAzureCredential(nil)
			if cerr != nil {
				return nil, fmt.Errorf("failed to create Azure credential: %w", cerr)
			}
			url := fmt.Sprintf("https://%s.blob.core.windows.net/", cfg.Account)
			client, err = azblob.NewClient(url, cred, nil)
		}
	case cfg.SASToken != "":
		// A SAS token needs the account name to build the endpoint.
		return nil, fmt.Errorf("azblob SAS auth requires AZURE_ACCOUNT")
	case cfg.ConnectionString != "":
		// Shared Key via a connection string is the least-preferred mechanism
		// (Microsoft recommends disallowing it), so it is used only when no
		// account-based credentials are configured.
		client, err = azblob.NewClientFromConnectionString(cfg.ConnectionString, nil)
	default:
		return nil, fmt.Errorf("azblob backend requires AZURE_ACCOUNT (DefaultAzureCredential), AZURE_ACCOUNT + AZURE_SAS_TOKEN (SAS), or AZURE_STORAGE_CONNECTION_STRING")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create Azure client: %w", err)
	}

	backend := &AzBlob{
		client:    client,
		container: containerName,
		prefix:    prefix,
		ctx:       ctx,
	}

	// Test container access by fetching its properties (mirrors the GCS bucket
	// Attrs check).
	if _, err := client.ServiceClient().NewContainerClient(containerName).GetProperties(ctx, nil); err != nil {
		return nil, fmt.Errorf("failed to access Azure container %s: %w", containerName, err)
	}

	return backend, nil
}

// Put stores an object in Azure Blob Storage.
func (a *AzBlob) Put(actionID, outputID []byte, body io.Reader, bodySize int64) error {
	key := a.actionIDToKey(actionID)

	meta := map[string]*string{
		"outputid": to.Ptr(hex.EncodeToString(outputID)),
		"size":     to.Ptr(strconv.FormatInt(bodySize, 10)),
		"time":     to.Ptr(strconv.FormatInt(time.Now().Unix(), 10)),
	}

	// UploadStream handles a nil/empty body fine, uploading an empty blob so a
	// subsequent Get still hits.
	if body == nil {
		body = strings.NewReader("")
	}

	_, err := a.client.UploadStream(a.ctx, a.container, key, body, &azblob.UploadStreamOptions{
		Metadata: meta,
	})
	if err != nil {
		return fmt.Errorf("failed to upload to Azure: %w", err)
	}

	return nil
}

// Get retrieves an object from Azure Blob Storage.
// Returns the object data as an io.ReadCloser that must be closed by the caller.
func (a *AzBlob) Get(actionID []byte) ([]byte, io.ReadCloser, int64, *time.Time, bool, error) {
	key := a.actionIDToKey(actionID)

	resp, err := a.client.DownloadStream(a.ctx, a.container, key, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return nil, nil, 0, nil, true, nil
		}
		return nil, nil, 0, nil, true, fmt.Errorf("failed to download from Azure: %w", err)
	}

	meta := normalizeMetadata(resp.Metadata)

	outputID, err := hex.DecodeString(meta["outputid"])
	if err != nil {
		if resp.Body != nil {
			resp.Body.Close()
		}
		return nil, nil, 0, nil, true, nil
	}

	// Fall back to the blob's content length / last-modified time if the metadata
	// trio is missing.
	size := parseSize(meta["size"], resp.ContentLength)
	putTime := parsePutTime(meta["time"], resp.LastModified)

	return outputID, resp.Body, size, putTime, false, nil
}

// normalizeMetadata lower-cases metadata keys, since Azure canonicalizes them
// (e.g. "Outputid") unlike GCS/S3.
func normalizeMetadata(raw map[string]*string) map[string]string {
	meta := make(map[string]string, len(raw))
	for k, v := range raw {
		if v != nil {
			meta[strings.ToLower(k)] = *v
		}
	}
	return meta
}

func parseSize(sizeStr string, fallback *int64) int64 {
	if size, err := strconv.ParseInt(sizeStr, 10, 64); err == nil {
		return size
	}
	if fallback != nil {
		return *fallback
	}
	return 0
}

func parsePutTime(timeStr string, fallback *time.Time) *time.Time {
	if timeStr != "" {
		if putTimeUnix, err := strconv.ParseInt(timeStr, 10, 64); err == nil {
			t := time.Unix(putTimeUnix, 0)
			return &t
		}
	}
	return fallback
}

// Close performs cleanup operations. The azblob client needs no explicit close.
func (a *AzBlob) Close() error {
	return nil
}

// Clear removes all entries from the cache in Azure Blob Storage.
func (a *AzBlob) Clear() error {
	pager := a.client.NewListBlobsFlatPager(a.container, &azblob.ListBlobsFlatOptions{
		Prefix: &a.prefix,
	})

	for pager.More() {
		page, err := pager.NextPage(a.ctx)
		if err != nil {
			return fmt.Errorf("failed to list Azure blobs: %w", err)
		}
		for _, blob := range page.Segment.BlobItems {
			if blob.Name == nil {
				continue
			}
			// Continue deleting other blobs even if one fails.
			_, _ = a.client.DeleteBlob(a.ctx, a.container, *blob.Name, nil)
		}
	}

	return nil
}

// actionIDToKey converts an actionID to an Azure blob name.
func (a *AzBlob) actionIDToKey(actionID []byte) string {
	hexID := hex.EncodeToString(actionID)
	if a.prefix != "" {
		return a.prefix + hexID
	}
	return hexID
}
