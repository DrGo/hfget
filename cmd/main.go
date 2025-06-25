package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	hfg "github.com/drgo/hfget"
	"golang.org/x/term"
)

var version = "6.1.4"

const (
	moveUp    = "\033[A"
	clearLine = "\r\033[2K"
)

// interface to facilitate testing
type downloader interface {
	FetchRepoInfo(ctx context.Context) (*hfg.RepoInfo, error)
	BuildPlan(ctx context.Context, repoInfo *hfg.RepoInfo) (*hfg.DownloadPlan, error)
	ExecutePlan(ctx context.Context, plan *hfg.DownloadPlan) error
}

type realDownloader struct {
	*hfg.Downloader
}

func (r *realDownloader) FetchRepoInfo(ctx context.Context) (*hfg.RepoInfo, error) {
	return r.Downloader.FetchRepoInfo(ctx)
}
func (r *realDownloader) BuildPlan(ctx context.Context, repoInfo *hfg.RepoInfo) (*hfg.DownloadPlan, error) {
	return r.Downloader.BuildPlan(ctx, repoInfo)
}
func (r *realDownloader) ExecutePlan(ctx context.Context, plan *hfg.DownloadPlan) error {
	return r.Downloader.ExecutePlan(ctx, plan)
}

type cliApp struct {
	out           io.Writer
	err           io.Writer
	isTerminal    bool
	terminalFd    int
	newDownloader func(repoName string, opts ...hfg.Option) downloader
}

func main() {
	fd := int(os.Stderr.Fd())
	app := &cliApp{
		out:        os.Stdout,
		err:        os.Stderr,
		isTerminal: term.IsTerminal(fd),
		terminalFd: fd,
		newDownloader: func(repoName string, opts ...hfg.Option) downloader {
			return &realDownloader{Downloader: hfg.New(repoName, opts...)}
		},
	}
	if err := app.run(os.Args[1:]); err != nil {
		log.New(app.err, "", 0).Printf("Error:\n%v", err)
		os.Exit(1)
	}
}

func (app *cliApp) run(args []string) error {
	log.SetOutput(app.err)
	log.SetFlags(0)

	var (
		isDatasetFlag   bool
		branch          string
		storage         string
		numConnections  int
		token           string
		skipSHA         bool
		maxRetries      int
		retryInterval   time.Duration
		quiet           bool
		force           bool
		useTree         bool
		includePatterns string
		excludePatterns string
		showVersion     bool
		verbose         bool
	)

	fs := flag.NewFlagSet("hfget", flag.ContinueOnError)
	fs.SetOutput(app.err)

	fs.BoolVar(&isDatasetFlag, "d", false, "Specify that the repo is a dataset")
	fs.StringVar(&branch, "b", envOrDefault("HFGET_BRANCH", "main"), "Branch of the model or dataset ($HFGET_BRANCH)")
	fs.StringVar(&storage, "s", envOrDefault("HFGET_STORAGE", "./"), "Storage path for downloads ($HFGET_STORAGE)")
	defaultConnections, _ := strconv.Atoi(envOrDefault("HFGET_CONCURRENT_CONNECTIONS", "5"))
	fs.IntVar(&numConnections, "c", defaultConnections, "Number of concurrent connections ($HFGET_CONCURRENT_CONNECTIONS)")
	fs.StringVar(&token, "t", envOrDefault("HFGET_TOKEN", ""), "HuggingFace Auth Token ($HFGET_TOKEN)")
	defaultSkipSHA, _ := strconv.ParseBool(envOrDefault("HFGET_SKIP_SHA", "false"))
	fs.BoolVar(&skipSHA, "k", defaultSkipSHA, "Skip SHA256 hash check ($HFGET_SKIP_SHA)")
	fs.IntVar(&maxRetries, "max-retries", 3, "Maximum number of retries")
	fs.DurationVar(&retryInterval, "retry-interval", 5*time.Second, "Interval between retries")
	fs.BoolVar(&quiet, "q", false, "Quiet mode (suppress progress display and prompts)")
	fs.BoolVar(&force, "f", false, "Force re-download of all files, implies quiet mode")
	fs.BoolVar(&useTree, "tree", false, "Use nested tree structure for output directory (e.g. 'org/model')")
	fs.StringVar(&includePatterns, "include", "", "Comma-separated glob patterns for files to download")
	fs.StringVar(&excludePatterns, "exclude", "", "Comma-separated glob patterns for files to exclude")
	fs.BoolVar(&showVersion, "version", false, "Show version information")
	fs.BoolVar(&verbose, "v", false, "Enable verbose diagnostic logging to stderr")

	fs.Usage = func() {
		fmt.Fprintf(app.err, "Usage: %s [options] model_or_dataset_name\n", os.Args[0])
		fmt.Fprintln(app.err, "Example: hfget TheBloke/Llama-2-7B-GGUF --include \"*.gguf\"")
		fmt.Fprintln(app.err, "Options:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return nil
	}

	if showVersion {
		fmt.Fprintf(app.out, "hfget version %s\n", version)
		return nil
	}

	if fs.NArg() < 1 {
		return errors.New("a model or dataset name argument is required")
	}
	repoName := fs.Arg(0)

	if !app.isTerminal || force {
		quiet = true
	}

	opts := []hfg.Option{
		hfg.WithBranch(branch), hfg.WithDestination(storage), hfg.WithConnections(numConnections),
	}
	if isDatasetFlag {
		opts = append(opts, hfg.AsDataset())
	}
	if token != "" {
		opts = append(opts, hfg.WithAuthToken(token))
	}
	if skipSHA {
		opts = append(opts, hfg.SkipSHACheck())
	}
	if force {
		opts = append(opts, hfg.WithForceRedownload())
	}
	if useTree {
		opts = append(opts, hfg.WithTreeStructure())
	}
	if includePatterns != "" {
		opts = append(opts, hfg.WithIncludePatterns(strings.Split(includePatterns, ",")))
	}
	if excludePatterns != "" {
		opts = append(opts, hfg.WithExcludePatterns(strings.Split(excludePatterns, ",")))
	}
	if verbose {
		opts = append(opts, hfg.WithVerboseOutput(app.err))
	}

	downloader := app.newDownloader(repoName, opts...)

	fmt.Fprintln(app.err, "Fetching repository information...")
	repoInfo, err := downloader.FetchRepoInfo(context.Background())
	if err != nil {
		return fmt.Errorf("could not fetch repository info: %w", err)
	}

	var wg sync.WaitGroup
	var progressChan chan hfg.Progress
	var totalAnalysisSize int64
	for _, s := range repoInfo.Siblings {
		if s.Type != "directory" {
			totalAnalysisSize += s.Size
		}
	}

	if !quiet {
		progressChan = make(chan hfg.Progress, numConnections*2)
		optsWithProgress := append(opts, hfg.WithProgressChannel(progressChan))
		downloader = app.newDownloader(repoName, optsWithProgress...)

		wg.Add(1)
		go func() {
			defer wg.Done()
			analysisDisplayProgress(app.err, progressChan, app.terminalFd, totalAnalysisSize)
		}()
	}

	plan, err := downloader.BuildPlan(context.Background(), repoInfo)
	if !quiet {
		close(progressChan)
		wg.Wait()
	}
	if err != nil {
		return fmt.Errorf("could not build download plan: %w", err)
	}

	if len(plan.FilesToDownload) == 0 {
		if len(plan.FilesToSkip) > 0 {
			log.Printf("%d files are already present and valid (Total Size: %s).", len(plan.FilesToSkip), formatBytes(plan.TotalSkipSize))
		}
		log.Println("Nothing to download.")

		if !force && !quiet {
			fmt.Fprint(app.err, "Would you like to force a re-download anyway? [y/N]: ")
			reader := bufio.NewReader(os.Stdin)
			input, _ := reader.ReadString('\n')
			if strings.TrimSpace(strings.ToLower(input)) == "y" {
				log.Println("Forcing re-download as requested...")
				for _, skippedFile := range plan.FilesToSkip {
					plan.FilesToDownload = append(plan.FilesToDownload, hfg.FileDownload{File: skippedFile.File, Reason: "forced re-download"})
				}
				plan.TotalDownloadSize += plan.TotalSkipSize
				plan.FilesToSkip = nil
				plan.TotalSkipSize = 0
			} else {
				log.Println("Exiting.")
				return nil
			}
		} else {
			return nil
		}
	}

	if !quiet {
		fmt.Fprintln(app.err, "----------------------------------------------------")
		fmt.Fprintf(app.err, "Repository:    %s\n", plan.Repo.ID)
		fmt.Fprintf(app.err, "Last Modified: %s\n", plan.Repo.LastModified.Format(time.RFC1123))
		fmt.Fprintln(app.err, "----------------------------------------------------")

		if len(plan.FilesToSkip) > 0 {
			fmt.Fprintf(app.err, "%d files already present and valid (Total: %s) will be skipped.\n", len(plan.FilesToSkip), formatBytes(plan.TotalSkipSize))
		}

		filesByReason := make(map[string][]hfg.FileDownload)
		for _, f := range plan.FilesToDownload {
			filesByReason[f.Reason] = append(filesByReason[f.Reason], f)
		}

		for reason, files := range filesByReason {
			fmt.Fprintf(app.err, "Files to download (Reason: %s):\n", reason)
			for _, file := range files {
				fmt.Fprintf(app.err, "  - %-60s (%s)\n", file.File.Path, formatBytes(file.File.Size))
			}
		}

		fmt.Fprintln(app.err, "----------------------------------------------------")
		fmt.Fprintf(app.err, "Total download size: %s\n", formatBytes(plan.TotalDownloadSize))
		fmt.Fprint(app.err, "Proceed with download? [y/N]: ")

		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		if strings.TrimSpace(strings.ToLower(input)) != "y" {
			log.Println("Download cancelled by user.")
			return nil
		}
	}

	if !quiet {
		progressChan = make(chan hfg.Progress, numConnections*2)
		optsWithProgress := append(opts, hfg.WithProgressChannel(progressChan))
		downloader = app.newDownloader(repoName, optsWithProgress...)

		wg.Add(1)
		go func() {
			defer wg.Done()
			downloadDisplayProgress(app.err, progressChan, app.terminalFd, plan)
		}()
	}

	log.Println("Starting download...")
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			log.Printf("Retrying after transient error (attempt %d/%d)...", i+1, maxRetries)
			time.Sleep(retryInterval)
		}
		lastErr = downloader.ExecutePlan(context.Background(), plan)
		if lastErr == nil || !isTransientError(lastErr) {
			break
		}
	}

	if !quiet {
		close(progressChan)
		wg.Wait()
	}

	if lastErr != nil {
		return lastErr
	}

	log.Printf("\nDownload of %s completed.", repoName)
	return nil
}

type fileProgressState struct {
	processedBytes int64
	totalSize      int64
	state          hfg.ProgressState
}

func analysisDisplayProgress(out io.Writer, progressChan <-chan hfg.Progress, fd int, totalAnalysisSize int64) {
	fileStates := make(map[string]*fileProgressState)
	var lastActiveFile string
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case pr, ok := <-progressChan:
			if !ok {
				fmt.Fprint(out, clearLine)
				fmt.Fprintln(out, "Analysis complete.")
				return
			}
			lastActiveFile = pr.Filepath

			state, exists := fileStates[pr.Filepath]
			if !exists {
				state = &fileProgressState{totalSize: pr.TotalSize}
				fileStates[pr.Filepath] = state
			}

			// For both verifying and verified/skipped, the value represents progress on that file
			state.processedBytes = pr.CurrentSize

		case <-ticker.C:
			width, _, _ := term.GetSize(fd)
			if width <= 0 {
				width = 90
			}

			var totalVerifiedBytes int64
			for _, state := range fileStates {
				totalVerifiedBytes += state.processedBytes
			}

			percent := 0.0
			if totalAnalysisSize > 0 {
				percent = (float64(totalVerifiedBytes) * 100) / float64(totalAnalysisSize)
			}

			if percent > 100.0 {
				percent = 100.0
			}

			fmt.Fprint(out, clearLine)
			fmt.Fprintf(out, "Analyzing (%.1f%%): Verifying %s", percent, truncateString(lastActiveFile, width-30))
		}
	}
}

type speedSample struct {
	t     time.Time
	bytes int64
}

func downloadDisplayProgress(out io.Writer, progressChan <-chan hfg.Progress, fd int, plan *hfg.DownloadPlan) {
	totalDownloadSize := plan.TotalDownloadSize
	var totalDownloaded, recentBytes int64
	fileStates := make(map[string]*fileProgressState)
	for _, f := range plan.FilesToDownload {
		fileStates[f.File.Path] = &fileProgressState{totalSize: f.File.Size}
	}

	downloadStartTime := time.Now()
	var speedSamples []speedSample
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var linesPrinted int

	for {
		select {
		case pr, ok := <-progressChan:
			if !ok {
				fmt.Fprint(out, clearLine)
				if linesPrinted > 1 {
					fmt.Fprint(out, moveUp+clearLine)
				}
				fmt.Fprint(out, "\r")
				fmt.Printf("Overall: 100.0%% (%s/%s) | Complete.\n\n", formatBytes(totalDownloadSize), formatBytes(totalDownloadSize))
				return
			}
			state, exists := fileStates[pr.Filepath]
			if !exists {
				continue
			}
			state.state = pr.State
			// main.go -> downloadDisplayProgress -> select -> case
			switch pr.State {
			case hfg.ProgressStateDownloading:
				// Calculate the number of new bytes since the last update
				if pr.CurrentSize > state.processedBytes {
					delta := pr.CurrentSize - state.processedBytes
					totalDownloaded += delta
					recentBytes += delta
				}
				// Set the file's progress to the new cumulative value
				state.processedBytes = pr.CurrentSize

			case hfg.ProgressStateComplete, hfg.ProgressStateVerified:
				// When a file finishes, ensure it's marked as 100%
				if state.processedBytes < state.totalSize {
					delta := state.totalSize - state.processedBytes
					totalDownloaded += delta
					// No need to add to recentBytes, as this is a finalization, not a speed measure
				}
				state.processedBytes = state.totalSize
			}
		case <-ticker.C:
			width, _, _ := term.GetSize(fd)
			if width <= 0 {
				width = 90
			}

			if linesPrinted > 0 {
				fmt.Fprint(out, clearLine)
				if linesPrinted > 1 {
					fmt.Fprint(out, moveUp+clearLine)
				}
				fmt.Fprint(out, "\r")
			}

			now := time.Now()
			if recentBytes > 0 {
				speedSamples = append(speedSamples, speedSample{t: now, bytes: recentBytes})
				recentBytes = 0
			}
			cutoff := now.Add(-5 * time.Second)
			firstValidIndex := -1
			for i, sample := range speedSamples {
				if !sample.t.Before(cutoff) {
					firstValidIndex = i
					break
				}
			}
			if firstValidIndex > 0 {
				speedSamples = speedSamples[firstValidIndex:]
			} else if firstValidIndex == -1 && len(speedSamples) > 0 && now.Sub(speedSamples[0].t) > 5*time.Second {
				speedSamples = nil
			}

			var currentSpeedBytes int64
			for _, sample := range speedSamples {
				currentSpeedBytes += sample.bytes
			}
			currentSpeed := float64(currentSpeedBytes) / 5.0

			elapsed := time.Since(downloadStartTime).Seconds()
			if elapsed < 0.1 {
				elapsed = 0.1
			}
			avgSpeed := float64(totalDownloaded) / elapsed

			overallPercent := 0.0
			if totalDownloadSize > 0 {
				overallPercent = (float64(totalDownloaded) * 100) / float64(totalDownloadSize)
			}
			line1 := fmt.Sprintf("Overall: %.1f%% (%s/%s) | Avg: %s | Current: %s",
				overallPercent, formatBytes(totalDownloaded), formatBytes(totalDownloadSize),
				formatSpeed(avgSpeed), formatSpeed(currentSpeed))

			var activeFile string
			var activeState *fileProgressState
			for _, f := range plan.FilesToDownload {
				state := fileStates[f.File.Path]
				if state != nil && state.state == hfg.ProgressStateDownloading {
					activeFile = f.File.Path
					activeState = state
					break
				}
			}

			var line2 string
			if activeState != nil && activeFile != "" {
				filePercent := 0.0
				if activeState.totalSize > 0 {
					filePercent = (float64(activeState.processedBytes) * 100) / float64(activeState.totalSize)
				}
				line2 = fmt.Sprintf("File: %s [%.1f%%]",
					truncateString(activeFile, width-20), filePercent)
			} else {
				line2 = "Finalizing..."
			}
			if len(line1) > width {
				line1 = line1[:width]
			}
			if len(line2) > width {
				line2 = line2[:width]
			}

			fmt.Fprintln(out, line1)
			fmt.Fprint(out, line2)
			linesPrinted = 2
		}
	}
}

func envOrDefault(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, hfg.ErrAuthentication) || errors.Is(err, hfg.ErrForbidden) || errors.Is(err, hfg.ErrNotFound) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary()
	}
	if strings.Contains(err.Error(), "i/o timeout") {
		return true
	}
	return false
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

func formatSpeed(s float64) string {
	if s < 1 {
		return "0"
	}
	return formatBytes(int64(s)) + "/s"
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return "..." + s[len(s)-maxLen+3:]
}
