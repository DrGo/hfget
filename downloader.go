package hfget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DownloadPlan holds a detailed summary of actions to be taken.
type DownloadPlan struct {
	Repo                 *RepoInfo
	FilesToDownloadMissing []HFFile // Files that don't exist locally
	FilesToDownloadInvalid []HFFile // Files that exist but are invalid
	TotalDownloadSize    int64
	FilesToSkip          []HFFile
	TotalSkipSize        int64
}

// FilesToDownload returns a combined slice of all files that need downloading.
func (dp *DownloadPlan) FilesToDownload() []HFFile {
	return append(dp.FilesToDownloadMissing, dp.FilesToDownloadInvalid...)
}

// Downloader is a client for downloading models from Hugging Face.
type Downloader struct {
	client              *http.Client
	logger              *log.Logger
	numConnections      int
	authToken           string
	skipSHA             bool
	forceRedownload     bool
	useTreeStructure    bool
	branch              string
	destinationBasePath string
	repoName            string
	isDataset           bool
	includePatterns     []string
	excludePatterns     []string
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
		logger:              log.New(io.Discard, "[hfget verbose] ", log.Ltime|log.Lmicroseconds),
		client: &http.Client{
			Timeout: 60 * time.Second,
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
	for _, opt := range opts {
		opt(d)
	}
	return d
}

func (d *Downloader) setLogger(w io.Writer) {
	d.logger.SetOutput(w)
}

func (d *Downloader) getModelPath(repoID string) string {
	var modelFolderName string
	if d.useTreeStructure {
		modelFolderName = repoID
	} else {
		modelFolderName = strings.ReplaceAll(repoID, "/", "_")
	}
	return filepath.Join(d.destinationBasePath, modelFolderName)
}

func (d *Downloader) GetDownloadPlan(ctx context.Context) (*DownloadPlan, error) {
	d.logger.Printf("Starting analysis for repo: %s, branch: %s", d.repoName, d.branch)
	repoInfo, err := d.fetchRepoInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get repo info: %w", err)
	}

	plan := &DownloadPlan{
		Repo: repoInfo,
	}

	modelPath := d.getModelPath(repoInfo.ID)
	d.logger.Printf("Target local path set to: %s", modelPath)
	if err := d.buildPlanRecursively(ctx, modelPath, "", repoInfo.Siblings, plan); err != nil {
		return nil, err
	}

	plan.TotalDownloadSize = 0
	for _, f := range plan.FilesToDownload() {
		plan.TotalDownloadSize += f.Size
	}

	d.logger.Printf("Analysis complete. Found %d files to download (%s) and %d valid files to skip (%s).",
		len(plan.FilesToDownload()), formatBytes(plan.TotalDownloadSize), len(plan.FilesToSkip), formatBytes(plan.TotalSkipSize))
	return plan, nil
}

func (d *Downloader) buildPlanRecursively(ctx context.Context, modelPath, subFolder string, files []HFFile, plan *DownloadPlan) error {
	for _, file := range files {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if file.Type == "directory" {
			d.logger.Printf("Scanning subdirectory: %s", file.Path)
			subFiles, err := d.fetchTree(ctx, file.Path)
			if err != nil {
				return err
			}
			if err := d.buildPlanRecursively(ctx, modelPath, file.Path, subFiles, plan); err != nil {
				return err
			}
			continue
		}

		if !d.shouldDownload(file.Path) {
			d.logger.Printf("Skipping file '%s' due to include/exclude filters.", file.Path)
			continue
		}

		fullPath := filepath.Join(modelPath, file.Path)
		if d.forceRedownload {
			d.logger.Printf("Forcing re-download for: %s", file.Path)
			plan.FilesToDownloadMissing = append(plan.FilesToDownloadMissing, file)
		} else {
			isValid, reason := d.isLocalFileValid(fullPath, file)
			if isValid {
				d.logger.Printf("File is already present and valid, skipping: %s", file.Path)
				plan.FilesToSkip = append(plan.FilesToSkip, file)
				plan.TotalSkipSize += file.Size
			} else {
				d.logger.Printf("File is missing or invalid (%s), planning download for: %s", reason, file.Path)
				if reason == "missing" {
					plan.FilesToDownloadMissing = append(plan.FilesToDownloadMissing, file)
				} else {
					plan.FilesToDownloadInvalid = append(plan.FilesToDownloadInvalid, file)
				}
			}
		}
	}
	return nil
}

func (d *Downloader) ExecutePlan(ctx context.Context, plan *DownloadPlan) error {
	modelPath := d.getModelPath(plan.Repo.ID)
	if err := os.MkdirAll(modelPath, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create root model directory %s: %w", modelPath, err)
	}

	for _, file := range plan.FilesToDownload() {
		d.logger.Printf("Starting download of: %s", file.Path)
		if err := d.downloadFile(ctx, modelPath, file); err != nil {
			return fmt.Errorf("failed to download %s: %w", file.Path, err)
		}

		d.sendProgress(file.Path, ProgressStateComplete, file.Size, file.Size, "Verifying...")

		fullPath := filepath.Join(modelPath, file.Path)
		verificationMethod, err := d.verifyLocalFile(fullPath, file, true) // Disable progress for final verification
		if err != nil {
			return fmt.Errorf("validation failed for downloaded file %s: %w", file.Path, err)
		}
		d.logger.Printf("Successfully verified '%s' via %s", file.Path, verificationMethod)
		d.sendProgress(file.Path, ProgressStateVerified, file.Size, file.Size, verificationMethod)
	}
	return nil
}

func (d *Downloader) verifyLocalFile(localPath string, remoteFile HFFile, disableProgress bool) (string, error) {
	d.logger.Printf("Verifying local file: %s", localPath)
	info, err := os.Stat(localPath)
	if err != nil {
		return "missing", err
	}
	if info.Size() != remoteFile.Size {
		d.logger.Printf("Size mismatch for %s: expected %d, got %d", localPath, remoteFile.Size, info.Size())
		return "size mismatch", fmt.Errorf("size mismatch")
	}
	if remoteFile.LFS.IsLFS && !d.skipSHA {
		d.logger.Printf("Performing SHA256 checksum for %s", localPath)
		if !disableProgress {
			d.sendProgress(remoteFile.Path, ProgressStateVerifying, 0, remoteFile.Size, "")
		}
		file, err := os.Open(localPath)
		if err != nil {
			return "read error", err
		}
		defer file.Close()
		hasher := sha256.New()
		var totalRead int64
		progressReader := &progressReader{
			r: file,
			callback: func(n int) {
				if !disableProgress {
					totalRead += int64(n)
					d.sendProgress(remoteFile.Path, ProgressStateVerifying, totalRead, remoteFile.Size, "")
				}
			},
		}
		if _, err := io.Copy(hasher, progressReader); err != nil {
			return "hashing error", fmt.Errorf("failed during hashing: %w", err)
		}
		actualChecksum := hex.EncodeToString(hasher.Sum(nil))
		if actualChecksum != remoteFile.LFS.Oid {
			d.logger.Printf("Checksum mismatch for %s", localPath)
			return "checksum mismatch", fmt.Errorf("checksum mismatch")
		}
		return "SHA256 Checksum", nil
	}
	return "File Size", nil
}

func (d *Downloader) isLocalFileValid(localPath string, remoteFile HFFile) (bool, string) {
	reason, err := d.verifyLocalFile(localPath, remoteFile, false)
	return err == nil, reason
}

func (d *Downloader) shouldDownload(path string) bool {
	for _, pattern := range d.excludePatterns {
		if matched, _ := filepath.Match(pattern, path); matched {
			return false
		}
	}
	if len(d.includePatterns) == 0 {
		return true
	}
	for _, pattern := range d.includePatterns {
		if matched, _ := filepath.Match(pattern, path); matched {
			return true
		}
	}
	return false
}

func (d *Downloader) downloadFile(ctx context.Context, modelPath string, file HFFile) error {
	downloadURL, err := d.resolveDownloadURL(ctx, file)
	if err != nil {
		return err
	}
	d.logger.Printf("Resolved download URL for '%s': %s", file.Path, downloadURL)
	fullPath := filepath.Join(modelPath, file.Path)
	tmpDir := filepath.Join(modelPath, ".tmp")
	if err := os.MkdirAll(tmpDir, os.ModePerm); err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	if !file.LFS.IsLFS || file.Size < int64(d.numConnections*1024*1024) {
		d.logger.Printf("Using single-threaded download for %s", file.Path)
		return d.downloadSingleThreaded(ctx, downloadURL, fullPath, file)
	}
	d.logger.Printf("Using multi-threaded download for %s (%d connections)", file.Path, d.numConnections)
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
			d.logger.Printf("Downloading chunk %d for %s (bytes %d-%d)", chunkIndex, file.Path, start, end)
			tmpFileName := filepath.Join(tmpDir, fmt.Sprintf("%s_%d.tmp", filepath.Base(file.Path), chunkIndex))
			if err := d.downloadChunk(ctx, downloadURL, tmpFileName, start, end, file); err != nil {
				errChan <- fmt.Errorf("chunk %d failed for %s: %w", chunkIndex, file.Path, err)
			}
		}(i, start, end)
	}
	wg.Wait()
	close(errChan)
	if err := <-errChan; err != nil {
		return err
	}
	d.logger.Printf("All chunks downloaded for %s, merging files...", file.Path)
	return mergeFiles(fullPath, tmpDir, filepath.Base(file.Path), d.numConnections)
}

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
	progressWriter := &progressWriter{
		w:        out,
		callback: func(n int) { d.sendProgress(file.Path, ProgressStateDownloading, int64(n), file.Size, "") },
	}
	_, err = io.Copy(progressWriter, resp.Body)
	return err
}

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
	progressWriter := &progressWriter{
		w:        out,
		callback: func(n int) { d.sendProgress(file.Path, ProgressStateDownloading, int64(n), file.Size, "") },
	}
	_, err = io.Copy(progressWriter, resp.Body)
	return err
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
		_ = os.Remove(tmpFileName)
	}
	return nil
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

type progressReader struct {
	r        io.Reader
	callback func(n int)
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.r.Read(p)
	if n > 0 && pr.callback != nil {
		pr.callback(n)
	}
	return
}

type progressWriter struct {
	w        io.Writer
	callback func(n int)
}

func (pw *progressWriter) Write(p []byte) (n int, err error) {
	n, err = pw.w.Write(p)
	if n > 0 && pw.callback != nil {
		pw.callback(n)
	}
	return
}
