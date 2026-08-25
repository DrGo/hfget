package hfget

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
)

const (
	resumeMetaVersion = 1
	resumeModeSingle  = "single"
	resumeModeMulti   = "multi"
)

// resumeMeta records which remote object a partial download belongs to so a
// later run can resume only if it is still the same file (size, git oid, LFS
// sha256, repo, branch, path).
type resumeMeta struct {
	Version     int    `json:"version"`
	Repo        string `json:"repo"`
	Branch      string `json:"branch"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	Oid         string `json:"oid"`
	LFSOid      string `json:"lfs_oid,omitempty"`
	Mode        string `json:"mode"`
	Connections int    `json:"connections,omitempty"`
}

func (d *Downloader) newResumeMeta(file HFFile, mode string, connections int) resumeMeta {
	return resumeMeta{
		Version:     resumeMetaVersion,
		Repo:        d.repoName,
		Branch:      d.branch,
		Path:        file.Path,
		Size:        file.Size,
		Oid:         file.Oid,
		LFSOid:      file.LFS.Oid,
		Mode:        mode,
		Connections: connections,
	}
}

func (m resumeMeta) matches(d *Downloader, file HFFile, mode string) bool {
	return m.Version == resumeMetaVersion &&
		m.Repo == d.repoName &&
		m.Branch == d.branch &&
		m.Path == file.Path &&
		m.Size == file.Size &&
		m.Oid == file.Oid &&
		m.LFSOid == file.LFS.Oid &&
		m.Mode == mode
}

func singleTmpPath(fullPath string) string {
	return fullPath + ".tmp"
}

func singleMetaPath(fullPath string) string {
	return fullPath + ".tmp.meta"
}

func (d *Downloader) multiChunkDir(modelPath string, file HFFile) string {
	return filepath.Join(modelPath, ".tmp", filepath.FromSlash(file.Path))
}

func multiMetaPath(chunkDir string) string {
	return filepath.Join(chunkDir, "resume.json")
}

func chunkTmpPath(chunkDir string, i int) string {
	return filepath.Join(chunkDir, fmt.Sprintf("chunk_%d.tmp", i))
}

func loadResumeMeta(path string) (resumeMeta, error) {
	var m resumeMeta
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	return m, nil
}

func writeResumeMeta(path string, m resumeMeta) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func fileSizeIfRegular(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return 0
	}
	return fi.Size()
}

func removeAllQuiet(paths ...string) {
	for _, p := range paths {
		_ = os.RemoveAll(p)
	}
}

func removeChunkDir(chunkDir string) {
	removeAllQuiet(chunkDir)
	dir := filepath.Dir(chunkDir)
	for {
		if err := os.Remove(dir); err != nil {
			return
		}
		if filepath.Base(dir) == ".tmp" {
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

func (d *Downloader) discardSinglePartial(fullPath string) {
	removeAllQuiet(singleTmpPath(fullPath), singleMetaPath(fullPath))
}

func (d *Downloader) discardMultiPartial(modelPath string, file HFFile) {
	removeAllQuiet(d.multiChunkDir(modelPath, file))
}

func (d *Downloader) discardPartials(modelPath string, file HFFile) {
	d.discardSinglePartial(filepath.Join(modelPath, file.Path))
	d.discardMultiPartial(modelPath, file)
}

func (d *Downloader) singlePartialSize(modelPath string, file HFFile) int64 {
	fullPath := filepath.Join(modelPath, file.Path)
	meta, err := loadResumeMeta(singleMetaPath(fullPath))
	if err != nil || !meta.matches(d, file, resumeModeSingle) {
		return 0
	}
	size := fileSizeIfRegular(singleTmpPath(fullPath))
	if size <= 0 || size > file.Size {
		return 0
	}
	return size
}

func (d *Downloader) multiPartialSize(modelPath string, file HFFile) (bytes int64, connections int) {
	chunkDir := d.multiChunkDir(modelPath, file)
	meta, err := loadResumeMeta(multiMetaPath(chunkDir))
	if err != nil || !meta.matches(d, file, resumeModeMulti) || meta.Connections < 1 {
		return 0, 0
	}
	var total int64
	for i := 0; i < meta.Connections; i++ {
		start, end := chunkByteRange(file.Size, meta.Connections, i)
		got := fileSizeIfRegular(chunkTmpPath(chunkDir, i))
		want := end - start + 1
		if got > want {
			return 0, 0
		}
		total += got
	}
	if total <= 0 {
		return 0, 0
	}
	return total, meta.Connections
}

func (d *Downloader) hasResumablePartial(modelPath string, file HFFile) bool {
	if d.singlePartialSize(modelPath, file) > 0 {
		return true
	}
	n, _ := d.multiPartialSize(modelPath, file)
	return n > 0
}

func (d *Downloader) existingResumeBytes(plan *DownloadPlan) int64 {
	if plan == nil {
		return 0
	}
	modelPath := d.getModelPath(plan.Repo.ID)
	var total int64
	for _, f := range plan.FilesToDownload {
		if n := d.singlePartialSize(modelPath, f.File); n > 0 {
			total += n
			continue
		}
		if n, _ := d.multiPartialSize(modelPath, f.File); n > 0 {
			total += n
		}
	}
	return total
}

func chunkByteRange(fileSize int64, connections, i int) (start, end int64) {
	chunkSize := fileSize / int64(connections)
	start = int64(i) * chunkSize
	end = start + chunkSize - 1
	if i == connections-1 {
		end = fileSize - 1
	}
	return start, end
}

func parseContentRangeStart(header string) (int64, bool) {
	var start, end, total int64
	n, err := fmt.Sscanf(header, "bytes %d-%d/%d", &start, &end, &total)
	if err != nil || n < 2 {
		return 0, false
	}
	return start, true
}

func hashFilePrefix(path string, n int64, hasher hash.Hash) error {
	if n <= 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	copied, err := io.Copy(hasher, io.LimitReader(f, n))
	if err != nil {
		return err
	}
	if copied != n {
		return fmt.Errorf("short read while hashing prefix: got %d, want %d", copied, n)
	}
	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
