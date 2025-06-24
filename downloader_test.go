package hfget

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	mockRepoID        = "test/repo"
	lfsFileContent    = "This is the content of the LFS file."
	// Corrected hash for lfsFileContent
	lfsFileSHA256     = "b9c44b024cd601ed9bc489243c66e18c164af0cf81a4ea2692dbc65498f8044d"
	badLfsFileContent = "This is bad LFS content with the wrong hash."
	nonLFSFileContent = "This is a regular file."
	nonLFSFileSHA1    = "a19b4561ba28351982b0b943d0e08dfde623e6e7" // Example SHA1
)

type mockFile struct {
	Path, Content, SHA256 string
	IsLFS                 bool
}

// setupMockServer now accepts a map of mock files to serve.
func setupMockServer(t *testing.T, files map[string]mockFile) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(nil)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tree/") {
			var treeJSON []string
			for _, f := range files {
				lfsPart := ""
				oid := nonLFSFileSHA1
				if f.IsLFS {
					lfsPart = fmt.Sprintf(`,"lfs":{"oid":"%s","size":%d}`, f.SHA256, len(f.Content))
					oid = f.SHA256
				}
				treeJSON = append(treeJSON, fmt.Sprintf(`{"type":"file","path":"%s","size":%d,"oid":"%s"%s}`, f.Path, len(f.Content), oid, lfsPart))
			}
			response := fmt.Sprintf(`[%s]`, strings.Join(treeJSON, ","))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(response))
			return
		}

		if strings.Contains(r.URL.Path, "/api/models/") && r.URL.Query().Get("revision") != "" {
			var siblingsJSON []string
			for _, f := range files {
				siblingsJSON = append(siblingsJSON, fmt.Sprintf(`{"rfilename":"%s"}`, f.Path))
			}
			response := fmt.Sprintf(`{"id":"%s","lastModified":"2023-01-01T00:00:00.000Z","siblings":[%s]}`, mockRepoID, strings.Join(siblingsJSON, ","))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(response))
			return
		}

		for _, f := range files {
			if strings.Contains(r.URL.Path, f.Path) {
				if f.IsLFS {
					if strings.Contains(r.URL.Path, "/resolve/") {
						location := fmt.Sprintf("%s/download/%s", server.URL, f.Path)
						w.Header().Set("Location", location)
						w.WriteHeader(http.StatusFound)
						return
					}
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(f.Content))
					return
				} else {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(f.Content))
					return
				}
			}
		}

		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Not Found"))
	})
	return server
}

func TestFetchRepoInfo(t *testing.T) {
	mockFiles := map[string]mockFile{
		"lfs.bin":     {Path: "lfs.bin", Content: lfsFileContent, SHA256: lfsFileSHA256, IsLFS: true},
		"regular.txt": {Path: "regular.txt", Content: nonLFSFileContent, IsLFS: false},
	}
	server := setupMockServer(t, mockFiles)
	defer server.Close()
	baseURL = server.URL

	d := New(mockRepoID)
	info, err := d.FetchRepoInfo(context.Background())
	requireNoError(t, err)

	if info.ID != mockRepoID {
		t.Errorf("Expected repo ID %s, got %s", mockRepoID, info.ID)
	}
	if len(info.Siblings) != 2 {
		t.Errorf("Expected 2 files in repo info, got %d", len(info.Siblings))
	}
}

func TestBuildPlan(t *testing.T) {
	repoInfo := &RepoInfo{
		ID:           mockRepoID,
		LastModified: time.Now(),
		Siblings: []HFFile{
			{Path: "lfs.bin", Type: "file", Size: int64(len(lfsFileContent)), LFS: HFLFS{IsLFS: true, Oid: lfsFileSHA256, Size: int64(len(lfsFileContent))}},
			{Path: "regular.txt", Type: "file", Size: int64(len(nonLFSFileContent))},
		},
	}

	t.Run("Full Download Plan", func(t *testing.T) {
		tmpDir := t.TempDir()
		d := New(mockRepoID, WithDestination(tmpDir))

		plan, err := d.BuildPlan(context.Background(), repoInfo)
		requireNoError(t, err)

		if len(plan.FilesToDownload) != 2 {
			t.Errorf("Expected 2 files to download, got %d", len(plan.FilesToDownload))
		}
		expectedSize := int64(len(lfsFileContent) + len(nonLFSFileContent))
		if plan.TotalDownloadSize != expectedSize {
			t.Errorf("Expected total size %d, got %d", expectedSize, plan.TotalDownloadSize)
		}
	})

	t.Run("Skip Existing Valid LFS File", func(t *testing.T) {
		tmpDir := t.TempDir()
		d := New(mockRepoID, WithDestination(tmpDir))
		
		repoPath := d.getModelPath(mockRepoID)
		requireNoError(t, os.MkdirAll(repoPath, 0755))
		lfsFilePath := filepath.Join(repoPath, "lfs.bin")
		requireNoError(t, os.WriteFile(lfsFilePath, []byte(lfsFileContent), 0644))

		plan, err := d.BuildPlan(context.Background(), repoInfo)
		requireNoError(t, err)

		if len(plan.FilesToDownload) != 1 {
			t.Errorf("Expected 1 file to download, got %d, files: %v", len(plan.FilesToDownload), plan.FilesToDownload)
		}
		if plan.FilesToDownload[0].File.Path != "regular.txt" {
			t.Errorf("Expected regular.txt to be in download plan, got %s", plan.FilesToDownload[0].File.Path)
		}
		if len(plan.FilesToSkip) != 1 {
			t.Errorf("Expected 1 file to be skipped, got %d", len(plan.FilesToSkip))
		}
	})

	t.Run("Plan to re-download invalid file", func(t *testing.T) {
		tmpDir := t.TempDir()
		d := New(mockRepoID, WithDestination(tmpDir))

		repoPath := d.getModelPath(mockRepoID)
		requireNoError(t, os.MkdirAll(repoPath, 0755))
		lfsFilePath := filepath.Join(repoPath, "lfs.bin")
		requireNoError(t, os.WriteFile(lfsFilePath, []byte("invalid content"), 0644))

		plan, err := d.BuildPlan(context.Background(), repoInfo)
		requireNoError(t, err)

		if len(plan.FilesToDownload) != 2 {
			t.Errorf("Expected 2 files to be in the plan for re-download, got %d", len(plan.FilesToDownload))
		}
	})
}

func TestExecutePlan(t *testing.T) {
	mockFiles := map[string]mockFile{
		"lfs.bin":     {Path: "lfs.bin", Content: lfsFileContent, SHA256: lfsFileSHA256, IsLFS: true},
		"regular.txt": {Path: "regular.txt", Content: nonLFSFileContent, IsLFS: false},
	}
	server := setupMockServer(t, mockFiles)
	defer server.Close()
	baseURL = server.URL

	tmpDir := t.TempDir()
	d := New(mockRepoID, WithDestination(tmpDir))
	info, err := d.FetchRepoInfo(context.Background())
	requireNoError(t, err)
	plan, err := d.BuildPlan(context.Background(), info)
	requireNoError(t, err)

	err = d.ExecutePlan(context.Background(), plan)
	requireNoError(t, err)

	repoPath := d.getModelPath(mockRepoID)
	verifyFileContent(t, filepath.Join(repoPath, "lfs.bin"), lfsFileContent)
	verifyFileContent(t, filepath.Join(repoPath, "regular.txt"), nonLFSFileContent)
}

func TestExecutePlan_ContinueOnError(t *testing.T) {
	// This test ensures that if one file fails validation, others still download.
	// We serve content for "bad.bin" that does NOT match its declared SHA256 hash.
	badFileContentFromServer := "this content does not match the hash"
	mockFiles := map[string]mockFile{
		"good.txt": {Path: "good.txt", Content: "This is good", IsLFS: false},
		"bad.bin":  {Path: "bad.bin", Content: badFileContentFromServer, SHA256: "this_is_a_deliberately_wrong_hash", IsLFS: true},
	}
	server := setupMockServer(t, mockFiles)
	defer server.Close()
	baseURL = server.URL

	tmpDir := t.TempDir()
	d := New(mockRepoID, WithDestination(tmpDir))
	info, err := d.FetchRepoInfo(context.Background())
	requireNoError(t, err)
	plan, err := d.BuildPlan(context.Background(), info) // All files will be planned for download
	requireNoError(t, err)

	err = d.ExecutePlan(context.Background(), plan)
	if err == nil {
		t.Fatal("Expected ExecutePlan to return an error for checksum mismatch, but it didn't")
	}
	if !strings.Contains(err.Error(), "validation failed for bad.bin") {
		t.Errorf("Expected error message to contain 'validation failed for bad.bin', but got: %v", err)
	}

	// But the good file should still have been downloaded correctly
	repoPath := d.getModelPath(mockRepoID)
	verifyFileContent(t, filepath.Join(repoPath, "good.txt"), "This is good")
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
}

func verifyFileContent(t *testing.T, path, expectedContent string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read file %s: %v", path, err)
	}
	if string(content) != expectedContent {
		t.Errorf("Content mismatch for %s. Expected '%s', got '%s'", path, expectedContent, string(content))
	}
}

