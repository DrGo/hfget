package hfget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// NOTE: All the `const` and `var` blocks that were duplicates of `api.go` have been removed.

const (
	mockRepoID                = "test/repo"
	lfsFileContent            = "This is the content of the LFS file."
	lfsFileSHA256             = "8319ca32884697978255959c203542614b439580554b423f812543fee73a365f"
	nonLFSFileContent         = "This is a regular file."
	nonLFSFileSHA256          = "a19b4561ba28351982b0b943d0e08dfde623e6e737c35539f55e42a9b319555f"
	subDirFileContent         = "This is a file in a subdirectory."
	subDirFileSHA256          = "c68c6759c735d483488f285d802148902894125952f417534789543e37130634"
	largeBenchmarkFileContent = "a"
)

type mockFile struct {
	Path, Content, SHA256 string
	IsLFS, IsSubDir       bool
}

// setupMockServer now accepts testing.TB, which works for both tests and benchmarks.
func setupMockServer(t testing.TB, files map[string]mockFile) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, fmt.Sprintf("/api/models/%s", mockRepoID)) && r.URL.Query().Get("revision") != "" {
			var siblingsJSON string
			for _, f := range files {
				if !f.IsSubDir {
					siblingsJSON += fmt.Sprintf(`{"rfilename": "%s"},`, f.Path)
				}
			}
			siblingsJSON += `{"rfilename": "sub"}`
			response := fmt.Sprintf(`{"id": "%s", "lastModified": "2023-01-01T00:00:00.000Z", "siblings": [%s]}`, mockRepoID, siblingsJSON)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(response))
			return
		}

		if strings.Contains(r.URL.Path, fmt.Sprintf("/api/models/%s/tree/main", mockRepoID)) {
			var treeJSON string
			if !strings.HasSuffix(r.URL.Path, "/sub") {
				for _, f := range files {
					if !f.IsSubDir {
						if f.IsLFS {
							treeJSON += fmt.Sprintf(`{"type": "file", "path": "%s", "size": %d, "lfs": {"oid": "%s", "size": %d}},`, f.Path, len(f.Content), f.SHA256, len(f.Content))
						} else {
							treeJSON += fmt.Sprintf(`{"type": "file", "path": "%s", "size": %d, "oid": "%s"},`, f.Path, len(f.Content), f.SHA256)
						}
					}
				}
				treeJSON += `{"type": "directory", "path": "sub"}`
			} else {
				for _, f := range files {
					if f.IsSubDir {
						treeJSON += fmt.Sprintf(`{"type": "file", "path": "%s", "size": %d, "oid": "%s"}`, f.Path, len(f.Content), f.SHA256)
					}
				}
			}
			response := fmt.Sprintf(`[%s]`, strings.TrimSuffix(treeJSON, ","))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(response))
			return
		}

		for _, f := range files {
			if f.IsLFS && strings.Contains(r.URL.Path, fmt.Sprintf("/resolve/main/%s", f.Path)) {
				w.Header().Set("Location", fmt.Sprintf("/download/%s", f.Path))
				w.WriteHeader(http.StatusFound)
				return
			}
			if f.IsLFS && strings.Contains(r.URL.Path, fmt.Sprintf("/download/%s", f.Path)) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(f.Content))
				return
			}
			if !f.IsLFS && strings.Contains(r.URL.Path, fmt.Sprintf("/raw/main/%s", f.Path)) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(f.Content))
				return
			}
		}

		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Not Found"))
	}))
}

func TestGetDownloadPlan(t *testing.T) {
	mockFiles := map[string]mockFile{
		"lfs.bin":     {Path: "lfs.bin", Content: lfsFileContent, SHA256: lfsFileSHA256, IsLFS: true},
		"regular.txt": {Path: "regular.txt", Content: nonLFSFileContent, SHA256: nonLFSFileSHA256, IsLFS: false},
		"sub/sub.txt": {Path: "sub/sub.txt", Content: subDirFileContent, SHA256: subDirFileSHA256, IsLFS: false, IsSubDir: true},
	}
	server := setupMockServer(t, mockFiles)
	defer server.Close()

	originalBaseURL := baseURL
	baseURL = server.URL
	defer func() { baseURL = originalBaseURL }()

	t.Run("Full Download Plan", func(t *testing.T) {
		tmpDir := t.TempDir()
		d := New(mockRepoID, WithDestination(tmpDir))

		plan, err := d.GetDownloadPlan(context.Background())
		if err != nil {
			t.Fatalf("GetDownloadPlan() failed: %v", err)
		}

		if len(plan.FilesToDownload) != 3 {
			t.Errorf("Expected 3 files to download, got %d", len(plan.FilesToDownload))
		}

		expectedSize := int64(len(lfsFileContent) + len(nonLFSFileContent) + len(subDirFileContent))
		if plan.TotalSize != expectedSize {
			t.Errorf("Expected total size %d, got %d", expectedSize, plan.TotalSize)
		}
	})

	t.Run("Skip Existing Valid File", func(t *testing.T) {
		tmpDir := t.TempDir()

		repoDir := filepath.Join(tmpDir, mockRepoID)
		requireNoError(t, os.MkdirAll(repoDir, 0755))
		requireNoError(t, os.WriteFile(filepath.Join(repoDir, "regular.txt"), []byte(nonLFSFileContent), 0644))

		d := New(mockRepoID, WithDestination(tmpDir))

		plan, err := d.GetDownloadPlan(context.Background())
		if err != nil {
			t.Fatalf("GetDownloadPlan() failed: %v", err)
		}

		if len(plan.FilesToDownload) != 2 {
			t.Errorf("Expected 2 files to download, got %d", len(plan.FilesToDownload))
		}

		for _, f := range plan.FilesToDownload {
			if f.Path == "regular.txt" {
				t.Error("regular.txt should have been skipped, but was included in the plan")
			}
		}
	})
}

func TestExecutePlan(t *testing.T) {
	mockFiles := map[string]mockFile{
		"lfs.bin":     {Path: "lfs.bin", Content: lfsFileContent, SHA256: lfsFileSHA256, IsLFS: true},
		"regular.txt": {Path: "regular.txt", Content: nonLFSFileContent, SHA256: nonLFSFileSHA256, IsLFS: false},
	}
	server := setupMockServer(t, mockFiles)
	defer server.Close()

	originalBaseURL := baseURL
	baseURL = server.URL
	defer func() { baseURL = originalBaseURL }()

	tmpDir := t.TempDir()
	d := New(mockRepoID, WithDestination(tmpDir))

	plan, err := d.GetDownloadPlan(context.Background())
	if err != nil {
		t.Fatalf("GetDownloadPlan() failed: %v", err)
	}

	err = d.ExecutePlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ExecutePlan() failed: %v", err)
	}

	repoPath := filepath.Join(tmpDir, plan.Repo.ID)
	verifyFileContent(t, filepath.Join(repoPath, "lfs.bin"), lfsFileContent)
	verifyFileContent(t, filepath.Join(repoPath, "regular.txt"), nonLFSFileContent)
}

func BenchmarkDownload(b *testing.B) {
	const fileSize = 10 * 1024 * 1024 // 10 MiB
	content := strings.Repeat(largeBenchmarkFileContent, fileSize)
	hasher := sha256.New()
	hasher.Write([]byte(content))
	hash := hex.EncodeToString(hasher.Sum(nil))

	mockFiles := map[string]mockFile{
		"largefile.bin": {Path: "largefile.bin", Content: content, SHA256: hash, IsLFS: true},
	}
	server := setupMockServer(b, mockFiles)
	defer server.Close()

	originalBaseURL := baseURL
	baseURL = server.URL
	defer func() { baseURL = originalBaseURL }()

	b.SetBytes(fileSize)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		tmpDir := b.TempDir()
		d := New(mockRepoID, WithDestination(tmpDir), WithConnections(4))
		plan, err := d.GetDownloadPlan(context.Background())
		if err != nil {
			b.Fatalf("GetDownloadPlan() failed: %v", err)
		}
		b.StartTimer()

		if err := d.ExecutePlan(context.Background(), plan); err != nil {
			b.Fatalf("ExecutePlan() failed: %v", err)
		}
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func verifyFileContent(t *testing.T, path, expectedContent string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read file %s: %v", path, err)
	}
	if string(content) != expectedContent {
		t.Errorf("Content mismatch for %s", path)
	}
}
