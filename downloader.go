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

// flattenTree recursively gets all files from a nested structure.
func (d *Downloader) flattenTree(files []HFFile) []HFFile {
	var flatList []HFFile
	for _, file := range files {
		if file.Type == "directory" {
			// In the new model, we fetch the full tree upfront, so we just recurse.
			// This part of the logic assumes that fetchRepoInfo gets the entire tree.
			// If not, API calls would be needed here. For now, assume a full tree.
		} else {
			flatList = append(flatList, file)
		}
	}
	return files // Assuming fetchRepoInfo provides a flat list of all files.
}

func (d *Downloader) processFileForPlan(ctx context.Context, modelPath string, file HFFile, plan *DownloadPlan) {
	if file.Type == "directory" {
		return // We only process files in this function
	}

	if !d.shouldDownload(file.Path) {
		d.logger.Printf("Skipping file '%s' due to include/exclude filters.", file.Path)
		d.sendProgress(file.Path, ProgressStateVerified, file.Size, file.Size, "filtered")
		return
	}

	fullPath := filepath.Join(modelPath, file.Path)
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
		err := d.downloadFile(ctx, modelPath, file)
		if err != nil {
			errStr := fmt.Sprintf("failed to download %s: %v", file.Path, err)
			log.Println(errStr)
			downloadErrors = append(downloadErrors, errStr)
			continue
		}

		d.sendProgress(file.Path, ProgressStateComplete, file.Size, file.Size, "Verifying...")

		fullPath := filepath.Join(modelPath, file.Path)
		verificationMethod, err := d.verifyLocalFile(fullPath, file, true)
		if err != nil {
			errStr := fmt.Sprintf("validation failed for %s: %v", file.Path, err)
			log.Println(errStr)
			downloadErrors = append(downloadErrors, errStr)
			continue
		}
		d.logger.Printf("Successfully verified '%s' via %s", verificationMethod, file.Path)
		d.sendProgress(file.Path, ProgressStateVerified, file.Size, file.Size, verificationMethod)
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

func (d *Downloader) downloadFile(ctx context.Context, modelPath string, file HFFile) error {
	downloadURL, err := d.resolveDownloadURL(ctx, file)
	if err != nil {
		return err
	}
	d.logger.Printf("Resolved download URL for '%s': %s", file.Path, downloadURL)
	fullPath := filepath.Join(modelPath, file.Path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return err
	}

	tmpDir := filepath.Join(modelPath, ".tmp")

	if !file.LFS.IsLFS || file.Size < int64(d.numConnections*1024*1024) {
		d.logger.Printf("Using single-threaded download for %s", file.Path)
		return d.downloadSingleThreaded(ctx, downloadURL, fullPath, file)
	}

	d.logger.Printf("Using multi-threaded download for %s (%d connections)", file.Path, d.numConnections)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	chunkSize := file.Size / int64(d.numConnections)
	var wg sync.WaitGroup
	errChan := make(chan error, d.numConnections)

	// Create a single, shared atomic counter for all chunks of this file.
	var downloadedBytes atomic.Int64

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
			// Pass the pointer to the shared counter into downloadChunk
			if err := d.downloadChunk(ctx, downloadURL, tmpFileName, start, end, file, &downloadedBytes); err != nil {
				errChan <- fmt.Errorf("chunk %d for %s failed: %w", chunkIndex, file.Path, err)
			}
		}(i, start, end)
	}
	wg.Wait()
	close(errChan)
	for err := range errChan {
		if err != nil {
			return err
		}
	}

	d.logger.Printf("All chunks downloaded for %s, merging files...", file.Path)
	return mergeFiles(fullPath, tmpDir, filepath.Base(file.Path), d.numConnections)
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

	// Create a new counter for this download.
	var downloadedBytes atomic.Int64

	idleReader := NewSafeIdleTimeoutReader(resp.Body, 60*time.Second)
	progressWriter := &progressWriter{
		filepath:     file.Path,
		totalSize:    file.Size,
		w:            out,
		d:            d,
		bytesWritten: &downloadedBytes, // Pass the counter's pointer.
	}

	_, err = io.Copy(progressWriter, idleReader)
	return err
}

func (d *Downloader) sendProgress(filepath string, state ProgressState, current, total int64, msg string) {
	if d.Progress == nil {
		return
	}

	// Create the progress update struct.
	progressUpdate := Progress{
		Filepath:    filepath,
		State:       state,
		CurrentSize: current,
		TotalSize:   total,
		Message:     msg,
	}

	// Use a non-blocking select to send the progress update.
	select {
	case d.Progress <- progressUpdate:
		// The update was sent successfully.
	default:
		// The channel was blocked (likely full or has no receiver).
		// We drop the update to prevent the downloader from hanging.
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
