package hfget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"
)

// DownloadPlan holds the information needed before a download begins.
type DownloadPlan struct {
	Repo            *RepoInfo
	FilesToDownload []HFFile
	TotalSize       int64
}

// Downloader is a client for downloading models from Hugging Face.
type Downloader struct {
	client              *http.Client
	numConnections      int
	authToken           string
	skipSHA             bool
	branch              string
	destinationBasePath string
	repoName            string
	isDataset           bool
	filter              []string
	Progress            chan<- Progress
}

// Option configures a Downloader.
type Option func(*Downloader)

// New creates a new Downloader with the given options.
func New(repoName string, opts ...Option) *Downloader {
	d := &Downloader{
		repoName:            repoName,
		numConnections:      5,
		branch:              "main",
		destinationBasePath: ".",
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	d.parseRepoName(repoName)
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// GetDownloadPlan inspects the remote repository and local disk to determine what needs to be downloaded.
func (d *Downloader) GetDownloadPlan(ctx context.Context) (*DownloadPlan, error) {
	repoInfo, err := d.fetchRepoInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get repo info: %w", err)
	}

	plan := &DownloadPlan{
		Repo: repoInfo,
	}

	modelPath := path.Join(d.destinationBasePath, repoInfo.ID)
	filesToDownload, totalSize, err := d.buildPlanRecursively(ctx, modelPath, "", repoInfo.Siblings)
	if err != nil {
		return nil, err
	}

	plan.FilesToDownload = filesToDownload
	plan.TotalSize = totalSize
	return plan, nil
}

// buildPlanRecursively is a helper to walk the file tree and build the list of files to download.
func (d *Downloader) buildPlanRecursively(ctx context.Context, modelPath, subFolder string, files []HFFile) ([]HFFile, int64, error) {
	var filesToDownload []HFFile
	var totalSize int64

	for _, file := range files {
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		default:
		}

		if file.Type == "directory" {
			subFiles, err := d.fetchTree(ctx, file.Path)
			if err != nil {
				return nil, 0, err
			}
			plannedSubFiles, plannedSize, err := d.buildPlanRecursively(ctx, modelPath, file.Path, subFiles)
			if err != nil {
				return nil, 0, err
			}
			filesToDownload = append(filesToDownload, plannedSubFiles...)
			totalSize += plannedSize
			continue
		}

		if d.shouldSkipByFilter(file.Path) {
			continue
		}

		fullPath := filepath.Join(modelPath, file.Path)
		if !d.isLocalFileValid(fullPath, file) {
			filesToDownload = append(filesToDownload, file)
			totalSize += file.Size
		}
	}
	return filesToDownload, totalSize, nil
}

// ExecutePlan downloads all files specified in a DownloadPlan.
func (d *Downloader) ExecutePlan(ctx context.Context, plan *DownloadPlan) error {
	modelPath := path.Join(d.destinationBasePath, plan.Repo.ID)
	if err := os.MkdirAll(modelPath, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create root model directory %s: %w", modelPath, err)
	}

	for _, file := range plan.FilesToDownload {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if dir := filepath.Dir(file.Path); dir != "." {
			if err := os.MkdirAll(filepath.Join(modelPath, dir), os.ModePerm); err != nil {
				return fmt.Errorf("failed to create subdirectory for %s: %w", file.Path, err)
			}
		}

		if err := d.downloadFile(ctx, modelPath, file); err != nil {
			return fmt.Errorf("failed to download %s: %w", file.Path, err)
		}

		fullPath := filepath.Join(modelPath, file.Path)
		if !d.isLocalFileValid(fullPath, file) {
			return fmt.Errorf("validation failed for downloaded file: %s", file.Path)
		}
		d.sendProgress(file.Path, ProgressStateComplete, file.Size, file.Size, "Download complete and verified")
	}
	return nil
}

// downloadFile manages the multi-threaded download of a single file.
func (d *Downloader) downloadFile(ctx context.Context, modelPath string, file HFFile) error {
	downloadURL, err := d.resolveDownloadURL(ctx, file)
	if err != nil {
		return err
	}

	fullPath := filepath.Join(modelPath, file.Path)
	tmpDir := filepath.Join(modelPath, ".tmp")
	if err := os.MkdirAll(tmpDir, os.ModePerm); err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	if !file.LFS.IsLFS || file.Size < int64(d.numConnections*1024*1024) {
		return d.downloadSingleThreaded(ctx, downloadURL, fullPath, file)
	}

	// Multi-threaded download
	chunkSize := file.Size / int64(d.numConnections)
	var wg sync.WaitGroup
	errChan := make(chan error, d.numConnections)

	for i := 0; i < d.numConnections; i++ {
		start := int64(i) * chunkSize
		end := start + chunkSize - 1
		if i == d.numConnections-1 {
			end = file.Size - 1
		}

		wg.Add(1)
		go func(chunkIndex int, start, end int64) {
			defer wg.Done()
			tmpFileName := filepath.Join(tmpDir, fmt.Sprintf("%s_%d.tmp", filepath.Base(file.Path), chunkIndex))
			err := d.downloadChunk(ctx, downloadURL, tmpFileName, start, end, file)
			if err != nil {
				errChan <- fmt.Errorf("chunk %d failed for %s: %w", chunkIndex, file.Path, err)
			}
		}(i, start, end)
	}

	wg.Wait()
	close(errChan)

	if err := <-errChan; err != nil {
		return err
	}

	return mergeFiles(fullPath, tmpDir, filepath.Base(file.Path), d.numConnections)
}

// downloadChunk downloads a specific byte range of a file.
func (d *Downloader) downloadChunk(ctx context.Context, url, tmpFileName string, start, end int64, file HFFile) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	if d.authToken != "" {
		req.Header.Add("Authorization", "Bearer "+d.authToken)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status code %d for %s", resp.StatusCode, url)
	}

	out, err := os.Create(tmpFileName)
	if err != nil {
		return err
	}
	defer out.Close()

	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := out.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			d.sendProgress(file.Path, ProgressStateDownloading, int64(n), file.Size, "")
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// downloadSingleThreaded downloads a file using a single connection.
func (d *Downloader) downloadSingleThreaded(ctx context.Context, url, fullPath string, file HFFile) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	if d.authToken != "" {
		req.Header.Add("Authorization", "Bearer "+d.authToken)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}

	out, err := os.Create(fullPath)
	if err != nil {
		return err
	}
	defer out.Close()

	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := out.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			d.sendProgress(file.Path, ProgressStateDownloading, int64(n), file.Size, "")
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *Downloader) sendProgress(filepath string, state ProgressState, current, total int64, msg string) {
	if d.Progress == nil {
		return
	}
	d.Progress <- Progress{
		Filepath:    filepath,
		State:       state,
		CurrentSize: current,
		TotalSize:   total,
		Message:     msg,
	}
}

func (d *Downloader) isLocalFileValid(localPath string, remoteFile HFFile) bool {
	info, err := os.Stat(localPath)
	if os.IsNotExist(err) {
		return false
	}
	if info.Size() != remoteFile.Size {
		return false
	}
	if d.skipSHA || !remoteFile.LFS.IsLFS {
		return true
	}
	return verifyChecksum(localPath, remoteFile.LFS.Oid) == nil
}

func verifyChecksum(filePath, expectedChecksum string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return err
	}
	actualChecksum := hex.EncodeToString(hasher.Sum(nil))
	if actualChecksum != expectedChecksum {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum)
	}
	return nil
}

func mergeFiles(outputFileName, tempDir, baseName string, numChunks int) error {
	outputFile, err := os.Create(outputFileName)
	if err != nil {
		return err
	}
	defer outputFile.Close()
	for i := 0; i < numChunks; i++ {
		tmpFileName := filepath.Join(tempDir, fmt.Sprintf("%s_%d.tmp", baseName, i))
		tmpFile, err := os.Open(tmpFileName)
		if err != nil {
			return err
		}
		if _, err := io.Copy(outputFile, tmpFile); err != nil {
			tmpFile.Close()
			return err
		}
		tmpFile.Close()
	}
	return nil
}
