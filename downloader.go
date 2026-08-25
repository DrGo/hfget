// Package hfget lightweight package for downloading models and datasets from HuggingFace
package hfget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// IdleTimeoutReader wraps an io.ReadCloser and is safe for concurrent use.
// It returns a timeout error if any single Read call takes longer than the timeout duration.
type IdleTimeoutReader struct {
	ctx     context.Context
	r       io.ReadCloser
	timeout time.Duration
}

// NewIdleTimeoutReader creates a new reader with a specified idle timeout.
func NewIdleTimeoutReader(ctx context.Context, r io.ReadCloser, timeout time.Duration) *IdleTimeoutReader {
	return &IdleTimeoutReader{
		ctx:     ctx,
		r:       r,
		timeout: timeout,
	}
}

// Read implements the io.Reader interface with an idle timeout.
func (r *IdleTimeoutReader) Read(p []byte) (n int, err error) {
	// Create a context that will be cancelled when the timeout is reached.
	ctx, cancel := context.WithTimeout(r.ctx, r.timeout)
	defer cancel() // Ensure the context resources are always released.

	type readResult struct {
		n   int
		err error
	}
	resultCh := make(chan readResult, 1)

	// Launch the blocking Read operation in a separate goroutine.
	go func() {
		n, err := r.r.Read(p)
		resultCh <- readResult{n, err}
	}()

	select {
	case <-ctx.Done(): // deadline execeded
		// Force the underlying Read to abort, unblocking the goroutine.
		r.r.Close()

		// Await goroutine exit to prevent background memory corruption on 'p'.
		<-resultCh
		return 0, ctx.Err()
	case result := <-resultCh:
		// The read operation completed successfully or with its own error.
		return result.n, result.err
	}
}

// Close implements the io.Closer interface.
func (r *IdleTimeoutReader) Close() error { return r.r.Close() }

// DownloadPlan holds a detailed summary of actions to be taken.
type DownloadPlan struct {
	Repo              *RepoInfo
	FilesToDownload   []FileDownload
	TotalDownloadSize int64
	FilesToSkip       []FileSkip
	TotalSkipSize     int64
}

// FileDownload represents a file to be downloaded and the reason.
type FileDownload struct {
	File   HFFile
	Reason string
}

// FileSkip represents a file to be skipped and the reason.
type FileSkip struct {
	File   HFFile
	Reason string
}

type Option func(*Downloader)

// Downloader is a client for downloading models from Hugging Face.
type Downloader struct {
	client              *http.Client
	logger              *log.Logger
	baseURL             string
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

func New(repoName string, opts ...Option) *Downloader {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	d := &Downloader{
		baseURL:             "https://huggingface.co",
		repoName:            repoName,
		numConnections:      5,
		branch:              "main",
		destinationBasePath: ".",
		logger:              log.New(io.Discard, "[hfget verbose] ", log.Ltime|log.Lmicroseconds),
		client: &http.Client{
			Transport: transport,
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

// ModelDir returns the local folder for a repo ID under base: "org_model",
// or "org/model" when tree is true.
func ModelDir(base, repoID string, tree bool) string {
	if !tree {
		repoID = strings.ReplaceAll(repoID, "/", "_")
	}
	return filepath.Join(base, repoID)
}

func (d *Downloader) getModelPath(repoID string) string {
	return ModelDir(d.destinationBasePath, repoID, d.useTreeStructure)
}

// FetchRepoInfo gets all remote file metadata from the Hugging Face API.
func (d *Downloader) FetchRepoInfo(ctx context.Context) (*RepoInfo, error) {
	d.logger.Printf("Fetching remote repository info for: %s, branch: %s", d.repoName, d.branch)
	return d.fetchRepoInfo(ctx)
}

// BuildPlan compares the remote repo info with local files to create a download plan.
func (d *Downloader) BuildPlan(ctx context.Context, repoInfo *RepoInfo) (*DownloadPlan, error) {
	d.logger.Printf("Building download plan by checking local files.")
	plan := &DownloadPlan{
		Repo: repoInfo,
	}

	modelPath := d.getModelPath(repoInfo.ID)
	d.logger.Printf("Target local path set to: %s", modelPath)

	allFiles := d.flattenTree(repoInfo.Siblings)

	for _, file := range allFiles {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		d.processFileForPlan(ctx, modelPath, file, plan)
	}

	for _, f := range plan.FilesToDownload {
		plan.TotalDownloadSize += f.File.Size
	}
	for _, f := range plan.FilesToSkip {
		plan.TotalSkipSize += f.File.Size
	}

	d.logger.Printf("Plan complete. Found %d files to download (%s) and %d valid files to skip (%s).",
		len(plan.FilesToDownload), formatBytes(plan.TotalDownloadSize), len(plan.FilesToSkip), formatBytes(plan.TotalSkipSize))
	return plan, nil
}

// flattenTree filters a list of HFFile entries, returning only the actual files.
func (d *Downloader) flattenTree(files []HFFile) []HFFile {
	var flatList []HFFile
	// The Hugging Face API provides a flat list of all files, so we just need
	// to filter out any entries that are explicitly marked as 'directory'.
	for _, file := range files {
		if file.Type != "directory" {
			flatList = append(flatList, file)
		}
	}
	return flatList // Correctly returns the filtered list of files
}

func (d *Downloader) processFileForPlan(ctx context.Context, modelPath string, file HFFile, plan *DownloadPlan) {
	if !d.shouldDownload(file.Path) {
		d.logger.Printf("Skipping file '%s' due to include/exclude filters.", file.Path)
		d.sendProgress(file.Path, ProgressStateVerified, file.Size, file.Size, "filtered")
		return
	}

	fullPath := filepath.Join(modelPath, file.Path)

	// First, get the absolute path of our intended destination root.
	absModelPath, err := filepath.Abs(modelPath)
	if err != nil {
		d.logger.Printf("Security check failed: could not determine absolute path for destination '%s': %v", modelPath, err)
		return
	}
	// Then, get the absolute path of the file we are about to write.
	absFullPath, err := filepath.Abs(fullPath)
	if err != nil {
		d.logger.Printf("Security check failed: could not determine absolute path for file '%s': %v", fullPath, err)
		return
	}
	// Finally, ensure the file's path is truly a child of the destination path.
	rel, err := filepath.Rel(absModelPath, absFullPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		d.logger.Printf("Security check failed: file '%s' attempts to write outside of destination directory. Skipping.", file.Path)
		return
	}
	if d.forceRedownload {
		d.logger.Printf("Forcing re-download for: %s", file.Path)
		plan.FilesToDownload = append(plan.FilesToDownload, FileDownload{File: file, Reason: "forced re-download"})
		d.sendProgress(file.Path, ProgressStateVerified, file.Size, file.Size, "forced")
		return
	}

	isValid, reason := d.isLocalFileValid(ctx, fullPath, file)
	if isValid {
		d.logger.Printf("File is already present and valid, skipping: %s", file.Path)
		plan.FilesToSkip = append(plan.FilesToSkip, FileSkip{File: file, Reason: reason})
		d.sendProgress(file.Path, ProgressStateVerified, file.Size, file.Size, reason)
	} else {
		d.logger.Printf("File is missing or invalid (%s), planning download for: %s", reason, file.Path)
		plan.FilesToDownload = append(plan.FilesToDownload, FileDownload{File: file, Reason: reason})
		d.sendProgress(file.Path, ProgressStateVerified, file.Size, file.Size, reason)
	}
}

func (d *Downloader) ExecutePlan(ctx context.Context, plan *DownloadPlan) error {
	if err := d.prepareOutputDirectory(plan.Repo.ID); err != nil {
		return err
	}
	modelPath := d.getModelPath(plan.Repo.ID)
	var downloadErrors []error
	for _, fileToDownload := range plan.FilesToDownload {
		// Check for context cancellation before starting each file
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("download plan cancelled: %w", err)
		}

		file := fileToDownload.File
		d.logger.Printf("Starting download of: %s", file.Path)

		calculatedChecksum, err := d.downloadFile(ctx, d.getModelPath(plan.Repo.ID), file)
		if err != nil {
			d.logger.Printf("failed to download %s: %v", file.Path, err)
			// FIX: Wrap the error to preserve the chain (e.g., context.DeadlineExceeded)
			downloadErrors = append(downloadErrors, fmt.Errorf("failed to download %s: %w", file.Path, err))
			continue
		}

		d.sendProgress(file.Path, ProgressStateComplete, file.Size, file.Size, "Verifying...")

		if calculatedChecksum != "" {
			if !d.skipSHA && file.LFS.IsLFS && calculatedChecksum != file.LFS.Oid {
				errStr := fmt.Errorf("validation failed for %s: checksum mismatch: expected %s, got %s", file.Path, file.LFS.Oid, calculatedChecksum)
				d.logger.Print(errStr)
				downloadErrors = append(downloadErrors, errStr)

				// FIX: Clean up the corrupted file to prevent it from being mistakenly used
				fullPath := filepath.Join(modelPath, file.Path)
				if removeErr := os.Remove(fullPath); removeErr != nil && !os.IsNotExist(removeErr) {
					d.logger.Printf("failed to remove corrupted file %s: %v", fullPath, removeErr)
				}
				continue
			}
			d.logger.Printf("Successfully verified '%s' via on-the-fly SHA256", file.Path)
			d.sendProgress(file.Path, ProgressStateVerified, file.Size, file.Size, "On-the-fly SHA256")
		} else {
			fullPath := filepath.Join(d.getModelPath(plan.Repo.ID), file.Path)
			verificationMethod, err := d.verifyLocalFile(ctx, fullPath, file, true)
			if err != nil {
				d.logger.Printf("validation failed for %s: %v", file.Path, err)
				downloadErrors = append(downloadErrors, fmt.Errorf("validation failed for %s: %w", file.Path, err))
				// FIX: Clean up the corrupted file
				if removeErr := os.Remove(fullPath); removeErr != nil && !os.IsNotExist(removeErr) {
					d.logger.Printf("failed to remove corrupted file %s: %v", fullPath, removeErr)
				}
				continue
			}
			d.logger.Printf("Successfully verified '%s' via %s", verificationMethod, file.Path)
			d.sendProgress(file.Path, ProgressStateVerified, file.Size, file.Size, verificationMethod)
		}
	}

	return aggregateErrors(downloadErrors)
}

// prepareOutputDirectory ensures the root model directory exists.
func (d *Downloader) prepareOutputDirectory(repoID string) error {
	modelPath := d.getModelPath(repoID)
	if err := os.MkdirAll(modelPath, 0o755); err != nil {
		return fmt.Errorf("failed to create root model directory %s: %w", modelPath, err)
	}
	return nil
}

// aggregateErrors joins multiple download errors into a single error.
// Crucially, it uses errors.Join to preserve the error chain, allowing
// callers (like the CLI retry loop) to use errors.Is/As on the underlying errors.
func aggregateErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	joinedErr := errors.Join(errs...)
	return fmt.Errorf("%d file(s) failed to download or verify: %w", len(errs), joinedErr)
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr *contextReader) Read(p []byte) (n int, err error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}

func (d *Downloader) verifyLocalFile(ctx context.Context, localPath string, remoteFile HFFile, disableProgress bool) (string, error) {
	d.logger.Printf("Verifying local file: %s", localPath)
	info, err := os.Stat(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "missing", err
		}
		return "stat error", err
	}
	if info.Size() != remoteFile.Size {
		d.logger.Printf("Size mismatch for %s: expected %d, got %d", localPath, remoteFile.Size, info.Size())
		return "size mismatch", fmt.Errorf("size mismatch: expected %d, got %d", remoteFile.Size, info.Size())
	}

	if remoteFile.LFS.IsLFS && !d.skipSHA {
		expectedChecksum := remoteFile.LFS.Oid
		d.logger.Printf("Performing SHA256 checksum for %s", localPath)

		var reader io.Reader
		file, err := os.Open(localPath)
		if err != nil {
			return "read error", err
		}
		defer file.Close()
		reader = file

		if !disableProgress {
			d.sendProgress(remoteFile.Path, ProgressStateVerifying, 0, remoteFile.Size, "")
			progressReader := &progressReader{
				r:         file,
				filepath:  remoteFile.Path,
				totalSize: remoteFile.Size,
				d:         d,
			}
			reader = progressReader
		}
		// Wrap the chosen reader with context awareness
		reader = &contextReader{ctx: ctx, r: reader}
		hasher := sha256.New()
		if _, err := io.Copy(hasher, reader); err != nil {
			return "hashing error", fmt.Errorf("failed during hashing: %w", err)
		}
		actualChecksum := hex.EncodeToString(hasher.Sum(nil))
		if actualChecksum != expectedChecksum {
			d.logger.Printf("Checksum mismatch for %s", localPath)
			return "checksum mismatch", fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum)
		}
		return "SHA256 Checksum", nil
	}
	return "File Size", nil
}

func (d *Downloader) isLocalFileValid(ctx context.Context, localPath string, remoteFile HFFile) (bool, string) {
	reason, err := d.verifyLocalFile(ctx, localPath, remoteFile, false)
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

func (d *Downloader) downloadMultiThreaded(ctx context.Context, url, fullPath, tmpDir string, file HFFile) (string, error) {
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	var downloadedBytes atomic.Int64
	chunkSize := file.Size / int64(d.numConnections)
	var wg sync.WaitGroup
	errChan := make(chan error, d.numConnections)

	for i := range d.numConnections {
		// FIX: Check context before spawning each chunk goroutine
		if err := ctx.Err(); err != nil {
			// Cancel remaining chunks by closing a local context or just returning
			return "", fmt.Errorf("multi-threaded download cancelled: %w", err)
		}
		start := int64(i) * chunkSize
		end := start + chunkSize - 1
		if i == d.numConnections-1 {
			end = file.Size - 1
		}
		wg.Add(1)
		go func(chunkIndex int, start, end int64) {
			defer wg.Done()
			tmpFileName := filepath.Join(tmpDir, fmt.Sprintf("%s_%d.tmp", filepath.Base(file.Path), chunkIndex))
			if err := d.downloadChunk(ctx, url, tmpFileName, start, end, file, &downloadedBytes); err != nil {
				errChan <- fmt.Errorf("chunk %d for %s failed: %w", chunkIndex, file.Path, err)
			}
		}(i, start, end)
	}
	wg.Wait()
	close(errChan)

	for err := range errChan {
		if err != nil {
			return "", err // Return on first chunk error
		}
	}

	d.logger.Printf("All chunks downloaded for %s, merging files...", file.Path)
	return mergeFiles(fullPath, tmpDir, filepath.Base(file.Path), d.numConnections, file, d.skipSHA)
}

// downloadFile returns a calculated checksum (if available) and an error.
func (d *Downloader) downloadFile(ctx context.Context, modelPath string, file HFFile) (string, error) {
	downloadURL, err := d.resolveDownloadURL(ctx, file)
	if err != nil {
		return "", err
	}
	d.logger.Printf("Resolved download URL for '%s': %s", file.Path, downloadURL)

	fullPath := filepath.Join(modelPath, file.Path)
	if err = os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return "", err
	}

	// High-level branching logic is now much clearer.
	if !file.LFS.IsLFS || file.Size < int64(d.numConnections*1024*1024) {
		d.logger.Printf("Using single-threaded download for %s", file.Path)
		return d.downloadSingleThreaded(ctx, downloadURL, fullPath, file)
	}

	d.logger.Printf("Using multi-threaded download for %s (%d connections)", file.Path, d.numConnections)
	tmpDir := filepath.Join(modelPath, ".tmp")
	checksum, err := d.downloadMultiThreaded(ctx, downloadURL, fullPath, tmpDir, file)
	// Return the checksum calculated during the merge phase
	return checksum, err
}

func (d *Downloader) downloadChunk(ctx context.Context, url, tmpFileName string, start, end int64, file HFFile, progressCounter *atomic.Int64) error {
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

	idleReader := NewIdleTimeoutReader(ctx, resp.Body, 60*time.Second)
	progressWriter := &progressWriter{
		filepath:     file.Path,
		totalSize:    file.Size,
		w:            out,
		d:            d,
		bytesWritten: progressCounter, // Use the passed-in shared counter
	}

	_, err = io.Copy(progressWriter, idleReader)
	return err
}

// downloadSingleThreaded now returns the calculated SHA256 checksum as a hex string.
func (d *Downloader) downloadSingleThreaded(ctx context.Context, url, fullPath string, file HFFile) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	if d.authToken != "" {
		req.Header.Add("Authorization", "Bearer "+d.authToken)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}
	tmpPath := fullPath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return "", err
	}

	var downloadedBytes atomic.Int64
	idleReader := NewIdleTimeoutReader(ctx, resp.Body, 60*time.Second)

	// Create a new hasher
	hasher := sha256.New()
	// Create a MultiWriter to write to both the file (out) and the hasher simultaneously.
	writer := io.MultiWriter(out, hasher)

	progressWriter := &progressWriter{
		filepath:     file.Path,
		totalSize:    file.Size,
		w:            writer, // Use the MultiWriter as the destination
		d:            d,
		bytesWritten: &downloadedBytes,
	}

	if _, err = io.Copy(progressWriter, idleReader); err != nil {
		out.Close()
		os.Remove(tmpPath)
		return "", err
	}
	out.Close()
	// Calculate the final checksum and return it.
	actualChecksum := hex.EncodeToString(hasher.Sum(nil))
	// Atomic rename
	if err := os.Rename(tmpPath, fullPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("failed to rename downloaded file: %w", err)
	}
	return actualChecksum, nil
}

// REPLACE the entire sendProgress function with this lock-free version:
func (d *Downloader) sendProgress(filepath string, state ProgressState, current, total int64, msg string) {
	if d.Progress == nil {
		return
	}

	progressUpdate := Progress{
		Filepath:    filepath,
		State:       state,
		CurrentSize: current,
		TotalSize:   total,
		Message:     msg,
	}

	// Non-blocking send. If the consumer (UI) is busy and the channel buffer is full,
	// we simply drop the update. The next update will catch up.
	// The consumer's 500ms ticker naturally handles UI throttling.
	select {
	case d.Progress <- progressUpdate:
	default:
	}
}

func mergeFiles(outputFileName, tempDir, baseName string, numChunks int, file HFFile, skipSHA bool) (string, error) {
	tmpOutName := outputFileName + ".merge-tmp"
	outputFile, err := os.Create(tmpOutName)
	if err != nil {
		return "", fmt.Errorf("failed to create merge temp file: %w", err)
	}

	// Create hasher and MultiWriter to calculate SHA256 on-the-fly
	hasher := sha256.New()
	writer := io.MultiWriter(outputFile, hasher)

	for i := range numChunks {
		tmpFileName := filepath.Join(tempDir, fmt.Sprintf("%s_%d.tmp", baseName, i))
		tmpFile, err := os.Open(tmpFileName)
		if err != nil {
			outputFile.Close()
			os.Remove(tmpOutName)
			return "", fmt.Errorf("failed to open chunk %d: %w", i, err)
		}

		if _, err := io.Copy(writer, tmpFile); err != nil {
			tmpFile.Close()
			outputFile.Close()
			os.Remove(tmpOutName)
			return "", fmt.Errorf("failed to copy chunk %d: %w", i, err)
		}
		tmpFile.Close()

		// Clean up the chunk file immediately after copying
		_ = os.Remove(tmpFileName)
	}

	// CRITICAL: Close the file handle before renaming to prevent Windows file-lock errors
	outputFile.Close()

	actualChecksum := hex.EncodeToString(hasher.Sum(nil))

	// Verify checksum before committing the file
	if file.LFS.IsLFS && !skipSHA && actualChecksum != file.LFS.Oid {
		os.Remove(tmpOutName)
		return "", fmt.Errorf("checksum mismatch during merge: expected %s, got %s", file.LFS.Oid, actualChecksum)
	}

	// Atomic rename
	if err := os.Rename(tmpOutName, outputFileName); err != nil {
		os.Remove(tmpOutName)
		return "", fmt.Errorf("failed to rename merged file: %w", err)
	}

	return actualChecksum, nil
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
	r         io.Reader
	filepath  string
	totalSize int64
	readBytes int64
	d         *Downloader
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.r.Read(p)
	if n > 0 && pr.d != nil {
		pr.readBytes += int64(n)
		pr.d.sendProgress(pr.filepath, ProgressStateVerifying, pr.readBytes, pr.totalSize, "")
	}
	return
}

type progressWriter struct {
	w            io.Writer
	filepath     string
	totalSize    int64
	d            *Downloader
	bytesWritten *atomic.Int64 // Pointer to a shared counter
}

func (pw *progressWriter) Write(p []byte) (n int, err error) {
	n, err = pw.w.Write(p)
	if n > 0 && pw.d != nil {
		// Add the number of bytes from this write to the shared counter.
		newTotal := pw.bytesWritten.Add(int64(n))
		// Send a progress update with the new CUMULATIVE total for the file.
		pw.d.sendProgress(pw.filepath, ProgressStateDownloading, newTotal, pw.totalSize, "")
	}
	return
}
