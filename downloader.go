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
		return
	}
	if d.hasResumablePartial(modelPath, file) {
		reason = "resume"
		d.logger.Printf("Planning resume of partial download for: %s", file.Path)
	} else {
		d.logger.Printf("File is missing or invalid (%s), planning download for: %s", reason, file.Path)
	}
	plan.FilesToDownload = append(plan.FilesToDownload, FileDownload{File: file, Reason: reason})
	d.sendProgress(file.Path, ProgressStateVerified, file.Size, file.Size, reason)
}

func (d *Downloader) ExecutePlan(ctx context.Context, plan *DownloadPlan) error {
	if err := d.prepareOutputDirectory(plan.Repo.ID); err != nil {
		return err
	}
	if err := d.ensureWritableSpace(plan); err != nil {
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

func (d *Downloader) ensureWritableSpace(plan *DownloadPlan) error {
	path := d.getModelPath(plan.Repo.ID)
	avail, err := AvailableSpace(path)
	if err != nil {
		d.logger.Printf("Could not determine available disk space at %s: %v (continuing without a space check)", path, err)
		return nil
	}
	needed := requiredDownloadSpace(plan, d.numConnections)
	if already := d.existingResumeBytes(plan); already > 0 {
		if already >= needed {
			needed = 0
		} else {
			needed -= already
		}
	}
	d.logger.Printf("Writable space at %s: available %s, required %s", path, formatBytes(avail), formatBytes(needed))
	return checkSpace(path, avail, needed)
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

func (d *Downloader) downloadMultiThreaded(ctx context.Context, url, fullPath, chunkDir string, file HFFile, connections int) (string, error) {
	if err := os.MkdirAll(chunkDir, 0o755); err != nil {
		return "", err
	}
	meta := d.newResumeMeta(file, resumeModeMulti, connections)
	if err := writeResumeMeta(multiMetaPath(chunkDir), meta); err != nil {
		return "", fmt.Errorf("failed to write resume metadata: %w", err)
	}

	var downloadedBytes atomic.Int64
	var wg sync.WaitGroup
	errChan := make(chan error, connections)

	for i := range connections {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("multi-threaded download cancelled: %w", err)
		}
		start, end := chunkByteRange(file.Size, connections, i)
		wg.Add(1)
		go func(chunkIndex int, start, end int64) {
			defer wg.Done()
			tmpFileName := chunkTmpPath(chunkDir, chunkIndex)
			if err := d.downloadChunk(ctx, url, tmpFileName, start, end, file, &downloadedBytes); err != nil {
				errChan <- fmt.Errorf("chunk %d for %s failed: %w", chunkIndex, file.Path, err)
			}
		}(i, start, end)
	}
	wg.Wait()
	close(errChan)

	for err := range errChan {
		if err != nil {
			return "", err
		}
	}

	d.logger.Printf("All chunks downloaded for %s, merging files...", file.Path)
	checksum, err := mergeFiles(fullPath, chunkDir, connections, file, d.skipSHA)
	if err != nil {
		if file.LFS.IsLFS && !d.skipSHA && strings.Contains(err.Error(), "checksum mismatch") {
			d.logger.Printf("Discarding partials for %s after checksum mismatch", file.Path)
			removeChunkDir(chunkDir)
		}
		return "", err
	}
	removeChunkDir(chunkDir)
	return checksum, nil
}

// downloadFile returns a calculated checksum (if available) and an error.
func (d *Downloader) downloadFile(ctx context.Context, modelPath string, file HFFile) (string, error) {
	fullPath := filepath.Join(modelPath, file.Path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return "", err
	}
	if d.forceRedownload {
		d.discardPartials(modelPath, file)
	}

	useMulti := d.shouldUseMultiThread(modelPath, file)
	if useMulti {
		d.logger.Printf("Using multi-threaded download for %s", file.Path)
	} else {
		d.logger.Printf("Using single-threaded download for %s", file.Path)
	}

	// Mark the file active before network I/O so a hung connect is visible.
	d.sendProgress(file.Path, ProgressStateDownloading, 0, file.Size, "connecting")

	downloadURL, err := d.resolveDownloadURL(ctx, file)
	if err != nil {
		return "", err
	}
	d.logger.Printf("Resolved download URL for '%s': %s", file.Path, downloadURL)

	if useMulti {
		d.discardSinglePartial(fullPath)
		connections := d.numConnections
		if n, stored := d.multiPartialSize(modelPath, file); n > 0 && stored > 0 {
			connections = stored
			d.logger.Printf("Resuming %s with %d connections from previous attempt", file.Path, connections)
		} else {
			d.discardMultiPartial(modelPath, file)
		}
		chunkDir := d.multiChunkDir(modelPath, file)
		return d.downloadMultiThreaded(ctx, downloadURL, fullPath, chunkDir, file, connections)
	}

	d.discardMultiPartial(modelPath, file)
	return d.downloadSingleThreaded(ctx, downloadURL, fullPath, file)
}

func (d *Downloader) shouldUseMultiThread(modelPath string, file HFFile) bool {
	if n, _ := d.multiPartialSize(modelPath, file); n > 0 {
		return true
	}
	if d.singlePartialSize(modelPath, file) > 0 {
		return false
	}
	return file.LFS.IsLFS && file.Size >= int64(d.numConnections)*1024*1024
}

func (d *Downloader) downloadChunk(ctx context.Context, url, tmpFileName string, start, end int64, file HFFile, progressCounter *atomic.Int64) error {
	want := end - start + 1
	existing := fileSizeIfRegular(tmpFileName)
	if existing > want {
		d.logger.Printf("Discarding oversized chunk %s (%d > %d)", tmpFileName, existing, want)
		_ = os.Remove(tmpFileName)
		existing = 0
	}
	if existing == want {
		progressCounter.Add(existing)
		d.sendProgress(file.Path, ProgressStateDownloading, progressCounter.Load(), file.Size, "resume")
		return nil
	}

	rangeStart := start + existing
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", rangeStart, end))
	if d.authToken != "" {
		req.Header.Add("Authorization", "Bearer "+d.authToken)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	fullFile := start == 0 && end == file.Size-1
	restart := existing == 0
	switch resp.StatusCode {
	case http.StatusPartialContent:
		if crStart, ok := parseContentRangeStart(resp.Header.Get("Content-Range")); ok && crStart != rangeStart {
			_ = os.Remove(tmpFileName)
			return fmt.Errorf("unexpected Content-Range start %d for %s (wanted %d)", crStart, file.Path, rangeStart)
		}
	case http.StatusOK:
		if !fullFile {
			if existing > 0 {
				_ = os.Remove(tmpFileName)
			}
			return fmt.Errorf("server ignored Range for chunk of %s", file.Path)
		}
		if existing > 0 {
			d.logger.Printf("Server ignored Range for %s, restarting chunk", file.Path)
			_ = os.Remove(tmpFileName)
			existing = 0
			restart = true
		}
	case http.StatusRequestedRangeNotSatisfiable:
		_ = os.Remove(tmpFileName)
		return fmt.Errorf("range not satisfiable for %s", file.Path)
	default:
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("unexpected status code %d for %s", resp.StatusCode, url)
		}
	}

	if existing > 0 && !restart {
		progressCounter.Add(existing)
		d.sendProgress(file.Path, ProgressStateDownloading, progressCounter.Load(), file.Size, "resume")
	}

	var out *os.File
	if restart || existing == 0 {
		out, err = os.Create(tmpFileName)
	} else {
		out, err = os.OpenFile(tmpFileName, os.O_WRONLY, 0o644)
		if err == nil {
			_, err = out.Seek(existing, io.SeekStart)
		}
	}
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
		bytesWritten: progressCounter,
	}

	_, err = io.Copy(progressWriter, idleReader)
	return err
}

func (d *Downloader) downloadSingleThreaded(ctx context.Context, url, fullPath string, file HFFile) (string, error) {
	tmpPath := singleTmpPath(fullPath)
	metaPath := singleMetaPath(fullPath)
	meta := d.newResumeMeta(file, resumeModeSingle, 0)

	existing := int64(0)
	if loaded, err := loadResumeMeta(metaPath); err == nil && loaded.matches(d, file, resumeModeSingle) {
		existing = fileSizeIfRegular(tmpPath)
		if existing > file.Size {
			d.logger.Printf("Discarding oversized partial for %s", file.Path)
			d.discardSinglePartial(fullPath)
			existing = 0
		}
	} else {
		if existing = fileSizeIfRegular(tmpPath); existing > 0 {
			d.logger.Printf("Discarding untagged or stale partial for %s", file.Path)
		}
		d.discardSinglePartial(fullPath)
		existing = 0
	}

	if existing == file.Size && existing > 0 {
		d.logger.Printf("Completing already-downloaded temp file for %s", file.Path)
		return d.commitSingleTmp(tmpPath, metaPath, fullPath, file)
	}

	if existing > 0 {
		d.logger.Printf("Resuming %s from offset %s", file.Path, formatBytes(existing))
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	if existing > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", existing))
	}
	if d.authToken != "" {
		req.Header.Add("Authorization", "Bearer "+d.authToken)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	restart := existing == 0
	switch {
	case existing > 0 && resp.StatusCode == http.StatusPartialContent:
		if crStart, ok := parseContentRangeStart(resp.Header.Get("Content-Range")); ok && crStart != existing {
			d.discardSinglePartial(fullPath)
			return "", fmt.Errorf("unexpected Content-Range start %d for %s (wanted %d)", crStart, file.Path, existing)
		}
	case existing > 0 && resp.StatusCode == http.StatusOK:
		d.logger.Printf("Server ignored Range for %s, restarting download", file.Path)
		d.discardSinglePartial(fullPath)
		existing = 0
		restart = true
	case existing > 0 && resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		d.discardSinglePartial(fullPath)
		return "", fmt.Errorf("range not satisfiable for %s", file.Path)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return "", fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}

	if err := writeResumeMeta(metaPath, meta); err != nil {
		return "", fmt.Errorf("failed to write resume metadata: %w", err)
	}

	hasher := sha256.New()
	if existing > 0 && !restart {
		if err := hashFilePrefix(tmpPath, existing, hasher); err != nil {
			return "", fmt.Errorf("failed to hash existing partial for %s: %w", file.Path, err)
		}
	}

	var out *os.File
	if restart || existing == 0 {
		out, err = os.Create(tmpPath)
	} else {
		out, err = os.OpenFile(tmpPath, os.O_WRONLY, 0o644)
		if err == nil {
			_, err = out.Seek(existing, io.SeekStart)
		}
	}
	if err != nil {
		return "", err
	}

	var downloadedBytes atomic.Int64
	downloadedBytes.Store(existing)
	d.sendProgress(file.Path, ProgressStateDownloading, existing, file.Size, "resume")

	idleReader := NewIdleTimeoutReader(ctx, resp.Body, 60*time.Second)
	writer := io.MultiWriter(out, hasher)
	progressWriter := &progressWriter{
		filepath:     file.Path,
		totalSize:    file.Size,
		w:            writer,
		d:            d,
		bytesWritten: &downloadedBytes,
	}

	_, err = io.Copy(progressWriter, idleReader)
	closeErr := out.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}

	got := fileSizeIfRegular(tmpPath)
	if got != file.Size {
		return "", fmt.Errorf("incomplete download for %s: got %d, want %d", file.Path, got, file.Size)
	}

	actualChecksum := hex.EncodeToString(hasher.Sum(nil))
	if file.LFS.IsLFS && !d.skipSHA && actualChecksum != file.LFS.Oid {
		d.discardSinglePartial(fullPath)
		return "", fmt.Errorf("checksum mismatch for %s: expected %s, got %s", file.Path, file.LFS.Oid, actualChecksum)
	}
	if err := os.Rename(tmpPath, fullPath); err != nil {
		return "", fmt.Errorf("failed to rename downloaded file: %w", err)
	}
	_ = os.Remove(metaPath)
	return actualChecksum, nil
}

func (d *Downloader) commitSingleTmp(tmpPath, metaPath, fullPath string, file HFFile) (string, error) {
	actualChecksum, err := sha256File(tmpPath)
	if err != nil {
		return "", err
	}
	if file.LFS.IsLFS && !d.skipSHA && actualChecksum != file.LFS.Oid {
		d.discardSinglePartial(fullPath)
		return "", fmt.Errorf("checksum mismatch for %s: expected %s, got %s", file.Path, file.LFS.Oid, actualChecksum)
	}
	if err := os.Rename(tmpPath, fullPath); err != nil {
		return "", fmt.Errorf("failed to rename downloaded file: %w", err)
	}
	_ = os.Remove(metaPath)
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

func mergeFiles(outputFileName, chunkDir string, numChunks int, file HFFile, skipSHA bool) (string, error) {
	tmpOutName := outputFileName + ".merge-tmp"
	outputFile, err := os.Create(tmpOutName)
	if err != nil {
		return "", fmt.Errorf("failed to create merge temp file: %w", err)
	}

	hasher := sha256.New()
	writer := io.MultiWriter(outputFile, hasher)

	for i := range numChunks {
		tmpFileName := chunkTmpPath(chunkDir, i)
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
	}

	outputFile.Close()

	actualChecksum := hex.EncodeToString(hasher.Sum(nil))

	if file.LFS.IsLFS && !skipSHA && actualChecksum != file.LFS.Oid {
		os.Remove(tmpOutName)
		return "", fmt.Errorf("checksum mismatch during merge: expected %s, got %s", file.LFS.Oid, actualChecksum)
	}

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
