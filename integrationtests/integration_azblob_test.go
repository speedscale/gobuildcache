package integrationtests

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCacheIntegrationAzBlob exercises the Azure Blob Storage backend end-to-end.
//
// It is gated on TEST_AZBLOB_CONTAINER and is skipped in -short mode. Authentication
// is provided through the environment, exactly as when running gobuildcache normally:
// set GOBUILDCACHE_AZURE_STORAGE_CONNECTION_STRING (recommended for the Azurite
// emulator or a real account) or GOBUILDCACHE_AZURE_ACCOUNT for DefaultAzureCredential.
//
// Local (offline) run against the Azurite emulator:
//
//	docker run -d --name azurite -p 10000:10000 \
//	    mcr.microsoft.com/azure-storage/azurite azurite-blob --blobHost 0.0.0.0
//	# create the container once, then:
//	export TEST_AZBLOB_CONTAINER=gobuildcache
//	export GOBUILDCACHE_AZURE_STORAGE_CONNECTION_STRING="<azurite dev connection string>"
//	go test ./integrationtests/ -run TestCacheIntegrationAzBlob -v
func TestCacheIntegrationAzBlob(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Azure integration test in short mode")
	}

	// Get Azure container from environment - required for Azure tests.
	azblobContainer := os.Getenv("TEST_AZBLOB_CONTAINER")
	if azblobContainer == "" {
		t.Fatal("TEST_AZBLOB_CONTAINER environment variable not set")
	}

	// Verify Azure credentials are available. Auth is resolved from the environment
	// (connection string takes precedence over account + DefaultAzureCredential).
	hasConnString := os.Getenv("GOBUILDCACHE_AZURE_STORAGE_CONNECTION_STRING") != "" ||
		os.Getenv("AZURE_STORAGE_CONNECTION_STRING") != ""
	hasAccount := os.Getenv("GOBUILDCACHE_AZURE_ACCOUNT") != "" ||
		os.Getenv("AZURE_ACCOUNT") != ""
	if !hasConnString && !hasAccount {
		t.Fatal("Azure credentials not set: provide GOBUILDCACHE_AZURE_STORAGE_CONNECTION_STRING or GOBUILDCACHE_AZURE_ACCOUNT")
	}

	currentDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get working directory: %v", err)
	}
	// Go up one directory since we're in integrationtests/
	workspaceDir := filepath.Join(currentDir, "..")

	var (
		buildDir   = filepath.Join(workspaceDir, "builds")
		binaryPath = filepath.Join(buildDir, "gobuildcache")
		testsDir   = filepath.Join(workspaceDir, "faketests")
		// Use a unique prefix to avoid conflicts with concurrent tests.
		containerPrefix = fmt.Sprintf("test-cache-%d", time.Now().Unix())
	)

	t.Logf("Using Azure container: %s with prefix: %s", azblobContainer, containerPrefix)

	t.Log("Step 1: Compiling the binary...")
	if err := os.MkdirAll(buildDir, 0755); err != nil {
		t.Fatalf("Failed to create build directory: %v", err)
	}

	buildCmd := exec.Command("go", "build", "-o", binaryPath, ".")
	buildCmd.Dir = workspaceDir
	buildOutput, err := buildCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to compile binary: %v\nOutput: %s", err, buildOutput)
	}
	t.Log("✓ Binary compiled successfully")

	// Use current environment for all commands (carries the Azure credentials).
	baseEnv := os.Environ()
	azblobEnv := baseEnv

	t.Log("Step 2: Clearing the Azure cache...")
	clearCmd := exec.Command(binaryPath, "clear",
		"-debug",
		"-backend=azblob",
		"-azblob-container="+azblobContainer,
		"-azblob-prefix="+containerPrefix+"/")
	clearCmd.Dir = workspaceDir
	clearCmd.Env = azblobEnv
	clearOutput, err := clearCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to clear Azure cache: %v\nOutput: %s", err, clearOutput)
	}
	t.Logf("✓ Azure cache cleared successfully: %s", strings.TrimSpace(string(clearOutput)))

	// Note: We don't start a separate server. Go's GOCACHEPROG will start the cache
	// server automatically when needed, using the environment variables we set.

	t.Log("Step 3: Running tests with Azure cache (first run)...")
	firstRunCmd := exec.Command("go", "test", "-v", testsDir)
	firstRunCmd.Dir = workspaceDir
	firstRunCmd.Env = append(baseEnv,
		"GOCACHEPROG="+binaryPath,
		"BACKEND_TYPE=azblob",
		"DEBUG=true",
		"AZBLOB_CONTAINER="+azblobContainer,
		"AZBLOB_PREFIX="+containerPrefix+"/")

	var firstRunOutput bytes.Buffer
	firstRunCmd.Stdout = &firstRunOutput
	firstRunCmd.Stderr = &firstRunOutput

	if err := firstRunCmd.Run(); err != nil {
		t.Fatalf("Tests failed on first run: %v\nOutput:\n%s", err, firstRunOutput.String())
	}

	t.Logf("First run output:\n%s", firstRunOutput.String())
	t.Log("✓ Tests passed on first run")

	if strings.Contains(firstRunOutput.String(), "(cached)") {
		t.Fatal("First run should not be cached, but found '(cached)' in output")
	}
	t.Log("✓ First run was not cached (as expected)")

	t.Log("Step 4: Running tests again to verify Azure caching...")
	secondRunCmd := exec.Command("go", "test", "-v", testsDir)
	secondRunCmd.Dir = workspaceDir
	secondRunCmd.Env = append(baseEnv,
		"GOCACHEPROG="+binaryPath,
		"BACKEND_TYPE=azblob",
		"DEBUG=true",
		"AZBLOB_CONTAINER="+azblobContainer,
		"AZBLOB_PREFIX="+containerPrefix+"/")

	var secondRunOutput bytes.Buffer
	secondRunCmd.Stdout = &secondRunOutput
	secondRunCmd.Stderr = &secondRunOutput

	if err := secondRunCmd.Run(); err != nil {
		t.Fatalf("Tests failed on second run: %v\nOutput:\n%s", err, secondRunOutput.String())
	}

	t.Logf("Second run output:\n%s", secondRunOutput.String())
	t.Log("✓ Tests passed on second run")

	// Verify that results were cached.
	if strings.Contains(secondRunOutput.String(), "(cached)") {
		t.Log("✓ Tests results were served from Azure cache!")
	} else {
		t.Fatalf("Tests did not use cached results from Azure. Expected to see '(cached)' in the output.\nOutput:\n%s", secondRunOutput.String())
	}

	// Final cleanup - clear the test data from Azure.
	t.Log("Step 5: Cleaning up Azure test data...")
	finalClearCmd := exec.Command(binaryPath, "clear",
		"-debug",
		"-backend=azblob",
		"-azblob-container="+azblobContainer,
		"-azblob-prefix="+containerPrefix+"/")
	finalClearCmd.Dir = workspaceDir
	finalClearCmd.Env = azblobEnv
	if output, err := finalClearCmd.CombinedOutput(); err != nil {
		t.Logf("Warning: Failed to clean up Azure test data: %v\nOutput: %s", err, output)
	} else {
		t.Log("✓ Azure test data cleaned up")
	}

	t.Log("=== All Azure integration tests passed! ===")
}
