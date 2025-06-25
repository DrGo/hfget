package hfget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// SafeIdleTimeoutReader wraps an io.ReadCloser and is safe for concurrent use.
// It returns a timeout error if any single Read call takes longer than the timeout duration.
type SafeIdleTimeoutReader struct {
	r       io.ReadCloser
	timeout time.Duration
}

// NewSafeIdleTimeoutReader creates a new reader with a specified idle timeout.
func NewSafeIdleTimeoutReader(r io.ReadCloser, timeout time.Duration) *SafeIdleTimeoutReader {
	return &SafeIdleTimeoutReader{
		r:       r,
		timeout: timeout,
	}
}

// Read implements the io.Reader interface with an idle timeout.
func (r *SafeIdleTimeoutReader) Read(p []byte) (n int, err error) {
	// Create a context that will be cancelled when the timeout is reached.
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
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
	case <-ctx.Done():
		// The context's deadline was exceeded. The most common error here is context.DeadlineExceeded.
		return 0, ctx.Err()
	case result := <-resultCh:
		// The read operation completed successfully or with its own error.
		return result.n, result.err
	}
}

// Close implements the io.Closer interface.
func (r *SafeIdleTimeoutReader) Close() error {
	return r.r.Close()
}

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
	progressState map[string]*progressState // Tracks update times per file
	progressMutex sync.Mutex                // Protects the progressState map
}

// Add this new struct definition as well, right after the Downloader struct.
type progressState struct {
	lastUpdated time.Time
}

type Option func(*Downloader)

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

func (d *Downloader) getModelPath(repoID string) string {
	var modelFolderName string
	if d.useTreeStructure {
		modelFolderName = repoID
	} else {
		modelFolderName = strings.ReplaceAll(repoID, "/", "_")
	}
	return filepath.Join(d.destinationBasePath, modelFolderName)
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
	if !strings.HasPrefix(absFullPath, absModelPath) {
		d.logger.Printf("Security check failed: file '%s' attempts to write outside of destination directory. Skipping.", file.Path)
		return
	}
	if d.forceRedownload {
		d.logger.Printf("Forcing re-download for: %s", file.Path)
		plan.FilesToDownload = append(plan.FilesToDownload, FileDownload{File: file, Reason: "forced re-download"})
		d.sendProgress(file.Path, ProgressStateVerified, file.Size, file.Size, "forced")
		return
	}

	isValid, reason := d.isLocalFileValid(fullPath, file)
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
	modelPath := d.getModelPath(plan.Repo.ID)
	if err := os.MkdirAll(modelPath, 0755); err != nil {
		return fmt.Errorf("failed to create root model directory %s: %w", modelPath, err)
	}

	var downloadErrors []string

	for _, fileToDownload := range plan.FilesToDownload {
		file := fileToDownload.File
		d.logger.Printf("Starting download of: %s", file.Path)

		calculatedChecksum, err := d.downloadFile(ctx, modelPath, file)
		if err != nil {
			d.logger.Printf("failed to download %s: %v", file.Path, err)
			downloadErrors = append(downloadErrors, fmt.Sprintf("failed to download %s: %v", file.Path, err))
			continue
		}

		d.sendProgress(file.Path, ProgressStateComplete, file.Size, file.Size, "Verifying...")

		if calculatedChecksum != "" {
			if !d.skipSHA && file.LFS.IsLFS && calculatedChecksum != file.LFS.Oid {
				errStr := fmt.Sprintf("validation failed for %s: checksum mismatch: expected %s, got %s", file.Path, file.LFS.Oid, calculatedChecksum)
				d.logger.Print(errStr)
				downloadErrors = append(downloadErrors, errStr)
				continue
			}
			d.logger.Printf("Successfully verified '%s' via on-the-fly SHA256", file.Path)
			d.sendProgress(file.Path, ProgressStateVerified, file.Size, file.Size, "On-the-fly SHA256")
		} else {
			fullPath := filepath.Join(modelPath, file.Path)
			verificationMethod, err := d.verifyLocalFile(fullPath, file, true)
			if err != nil {
				d.logger.Printf("validation failed for %s: %v", file.Path, err)
				downloadErrors = append(downloadErrors, fmt.Sprintf("validation failed for %s: %v", file.Path, err))
				continue
			}
			d.logger.Printf("Successfully verified '%s' via %s", verificationMethod, file.Path)
			d.sendProgress(file.Path, ProgressStateVerified, file.Size, file.Size, verificationMethod)
		}
	}

	if len(downloadErrors) > 0 {
		return fmt.Errorf("%d file(s) failed to download or verify:\n- %s", len(downloadErrors), strings.Join(downloadErrors, "\n- "))
	}

	return nil
}

func (d *Downloader) verifyLocalFile(localPath string, remoteFile HFFile, disableProgress bool) (string, error) {
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
// Add this new function to downloader.go

func (d *Downloader) downloadMultiThreaded(ctx context.Context, url, fullPath, tmpDir string, file HFFile) error {
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	var downloadedBytes atomic.Int64
	chunkSize := file.Size / int64(d.numConnections)
	var wg sync.WaitGroup
	errChan := make(chan error, d.numConnections)

	for i := range d.numConnections {
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
			return err // Return on first chunk error
		}
	}

	d.logger.Printf("All chunks downloaded for %s, merging files...", file.Path)
	return mergeFiles(fullPath, tmpDir, filepath.Base(file.Path), d.numConnections)
}

// downloadFile now returns a calculated checksum (if available) and an error.
// Replace the existing downloadFile function with this refactored version.

func (d *Downloader) downloadFile(ctx context.Context, modelPath string, file HFFile) (string, error) {
	downloadURL, err := d.resolveDownloadURL(ctx, file)
	if err != nil {
		return "", err
	}
	d.logger.Printf("Resolved download URL for '%s': %s", file.Path, downloadURL)

	fullPath := filepath.Join(modelPath, file.Path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return "", err
	}

	// High-level branching logic is now much clearer.
	if !file.LFS.IsLFS || file.Size < int64(d.numConnections*1024*1024) {
		d.logger.Printf("Using single-threaded download for %s", file.Path)
		return d.downloadSingleThreaded(ctx, downloadURL, fullPath, file)
	} 
	
	d.logger.Printf("Using multi-threaded download for %s (%d connections)", file.Path, d.numConnections)
	tmpDir := filepath.Join(modelPath, ".tmp")
	err = d.downloadMultiThreaded(ctx, downloadURL, fullPath, tmpDir, file)
	// Return empty checksum, signaling that post-download verification is needed.
	return "", err
}
// Add progressCounter *atomic.Int64 to the function signature
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

	idleReader := NewSafeIdleTimeoutReader(resp.Body, 60*time.Second)
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
	out, err := os.Create(fullPath)
	if err != nil {
		return "", err
	}
	defer out.Close()

	var downloadedBytes atomic.Int64
	idleReader := NewSafeIdleTimeoutReader(resp.Body, 60*time.Second)

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
		return "", err
	}

	// Calculate the final checksum and return it.
	actualChecksum := hex.EncodeToString(hasher.Sum(nil))
	return actualChecksum, nil
}
func (d *Downloader) sendProgress(filepath string, state ProgressState, current, total int64, msg string) {
	if d.Progress == nil {
		return
	}

	throttleInterval := 100 * time.Millisecond

	d.progressMutex.Lock()
	// Initialize the map on first use.
	if d.progressState == nil {
		d.progressState = make(map[string]*progressState)
	}
	// Get or create a state tracker for this specific file.
	if _, ok := d.progressState[filepath]; !ok {
		d.progressState[filepath] = &progressState{}
	}
	fileState := d.progressState[filepath]

	isFinalState := (state == ProgressStateComplete || state == ProgressStateVerified)
	// --- NEW LOGIC HERE ---
	// Also consider a download 100% complete as a final, non-throttled state.
	isDownloadComplete := (state == ProgressStateDownloading && current == total)

	// Throttle the update if it's not a final state, not a 100% download update,
	// not the first update, AND not enough time has passed.
	if !isFinalState && !isDownloadComplete && !fileState.lastUpdated.IsZero() && time.Since(fileState.lastUpdated) < throttleInterval {
		d.progressMutex.Unlock()
		return // Skip sending this update.
	}

	// If we are sending, update the timestamp.
	fileState.lastUpdated = time.Now()
	d.progressMutex.Unlock()


	progressUpdate := Progress{
		Filepath:    filepath,
		State:       state,
		CurrentSize: current,
		TotalSize:   total,
		Message:     msg,
	}

	// Use the non-blocking send.
	select {
	case d.Progress <- progressUpdate:
		// The update was sent successfully.
	default:
		// The channel was blocked. Drop the update to prevent hanging.
	}
}
func mergeFiles(outputFileName, tempDir, baseName string, numChunks int) error {
	outputFile, err := os.Create(outputFileName)
	if err != nil {
		return err
	}
	defer outputFile.Close()
	for i := range numChunks {
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
