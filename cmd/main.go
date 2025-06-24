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

var version = "5.8.0"

type downloader interface {
	GetDownloadPlan(ctx context.Context) (*hfg.DownloadPlan, error)
	ExecutePlan(ctx context.Context, plan *hfg.DownloadPlan) error
}

type realDownloader struct {
	*hfg.Downloader
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
	isTerm := term.IsTerminal(fd)

	app := &cliApp{
		out:        os.Stdout,
		err:        os.Stderr,
		isTerminal: isTerm,
		terminalFd: fd,
		newDownloader: func(repoName string, opts ...hfg.Option) downloader {
			return &realDownloader{Downloader: hfg.New(repoName, opts...)}
		},
	}
	if err := app.run(os.Args[1:]); err != nil {
		log.New(app.err, "", 0).Fatalf("Error: %v", err)
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
		timeout         time.Duration
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
	fs.DurationVar(&timeout, "timeout", 60*time.Second, "Timeout for network requests")

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
		fs.Usage()
		return errors.New("a model or dataset name argument is required")
	}
	repoName := fs.Arg(0)

	if !app.isTerminal {
		quiet = true
	}

	for {
		if force {
			quiet = true
		}

		opts := []hfg.Option{
			hfg.WithBranch(branch), hfg.WithDestination(storage), hfg.WithConnections(numConnections),
			hfg.WithTimeout(timeout),
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

		var wg sync.WaitGroup
		var progressChan chan hfg.Progress
		if !quiet {
			progressChan = make(chan hfg.Progress)
			opts = append(opts, hfg.WithProgressChannel(progressChan))
			wg.Add(1)
			go func() {
				defer wg.Done()
				displayProgressStdLib(app.err, progressChan, app.terminalFd)
			}()
		}

		downloader := app.newDownloader(repoName, opts...)

		if !quiet || verbose {
			log.Println("Analyzing repository...")
		}
		plan, err := downloader.GetDownloadPlan(context.Background())
		if err != nil {
			if !quiet {
				close(progressChan)
				wg.Wait()
			}
			return fmt.Errorf("could not analyze repository: %w", err)
		}

		if len(plan.FilesToDownload()) == 0 {
			if !quiet {
				close(progressChan)
				wg.Wait()
			}
			if len(plan.FilesToSkip) > 0 {
				log.Printf("%d files are already present and valid (Total Size: %s).", len(plan.FilesToSkip), formatBytes(plan.TotalSkipSize))
			}
			log.Println("Nothing to download.")
			if force {
				return nil
			}
			fmt.Fprint(app.err, "Would you like to force a re-download anyway? [y/N]: ")
			reader := bufio.NewReader(os.Stdin)
			input, _ := reader.ReadString('\n')
			if strings.TrimSpace(strings.ToLower(input)) == "y" {
				force = true
				log.Println("Forcing re-download as requested...")
				continue
			} else {
				log.Println("Exiting.")
				return nil
			}
		}

		if !quiet {
			fmt.Fprintf(app.err, "\r\033[2K")
			fmt.Fprintln(app.err, "----------------------------------------------------")
			fmt.Fprintf(app.err, "Repository:    %s\n", plan.Repo.ID)
			fmt.Fprintf(app.err, "Last Modified: %s\n", plan.Repo.LastModified.Format(time.RFC1123))
			fmt.Fprintln(app.err, "----------------------------------------------------")
			if len(plan.FilesToSkip) > 0 {
				fmt.Fprintf(app.err, "%d files already present and valid (Total: %s) will be skipped.\n", len(plan.FilesToSkip), formatBytes(plan.TotalSkipSize))
			}
			if len(plan.FilesToDownloadMissing) > 0 {
				fmt.Fprintln(app.err, "New files to be downloaded:")
				for _, file := range plan.FilesToDownloadMissing {
					fmt.Fprintf(app.err, "  - %-60s (%s)\n", file.Path, formatBytes(file.Size))
				}
			}
			if len(plan.FilesToDownloadInvalid) > 0 {
				fmt.Fprintln(app.err, "Invalid local files to be re-downloaded:")
				for _, file := range plan.FilesToDownloadInvalid {
					fmt.Fprintf(app.err, "  - %-60s (%s)\n", file.Path, formatBytes(file.Size))
				}
			}
			fmt.Fprintln(app.err, "----------------------------------------------------")
			fmt.Fprintf(app.err, "Total download size: %s\n", formatBytes(plan.TotalDownloadSize))
			fmt.Fprint(app.err, "Proceed with download? [y/N]: ")

			reader := bufio.NewReader(os.Stdin)
			input, _ := reader.ReadString('\n')
			if strings.TrimSpace(strings.ToLower(input)) != "y" {
				log.Println("Download cancelled by user.")
				if !quiet {
					close(progressChan)
					wg.Wait()
				}
				return nil
			}
		}

		log.Println("Starting download...")
		for i := 0; i < maxRetries; i++ {
			if i > 0 {
				fmt.Fprintln(app.err)
				log.Printf("Attempt %d of %d after error: %v", i+1, maxRetries, err)
				time.Sleep(retryInterval)
			}
			err = downloader.ExecutePlan(context.Background(), plan)
			if err == nil {
				break
			}
			if !isTransientError(err) {
				return fmt.Errorf("a fatal error occurred: %w", err)
			}
		}

		if !quiet {
			close(progressChan)
			wg.Wait()
		}

		if err != nil {
			return fmt.Errorf("failed to download after %d attempts: %w", err)
		}

		log.Printf("Download of %s completed successfully.", repoName)
		break
	}
	return nil
}

func envOrDefault(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func isTransientError(err error) bool {
	if errors.Is(err, hfg.ErrAuthentication) || errors.Is(err, hfg.ErrForbidden) || errors.Is(err, hfg.ErrNotFound) {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

type fileState struct {
	total, downloadedBytes, verifiedBytes int64
}

func displayProgressStdLib(out io.Writer, progressChan <-chan hfg.Progress, fd int) {
	fileProgress := make(map[string]*fileState)
	var totalDownloadSize, totalDownloaded, totalVerifiedSize, totalVerified int64
	var downloadStartTime time.Time
	var recentBytes int64
	var recentTime time.Time
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var lastActiveFile string
	var currentPhase string = "Analyzing"
	spinner := []rune{'|', '/', '-', '\\'}
	spinnerIndex := 0
	linesPrinted := 0

	const (
		moveUp    = "\033[A"
		clearLine = "\r\033[2K"
	)

	for {
		select {
		case pr, ok := <-progressChan:
			if !ok {
				if linesPrinted > 0 {
					fmt.Fprint(out, strings.Repeat(moveUp, linesPrinted-1), clearLine)
				}
				return
			}
			if _, exists := fileProgress[pr.Filepath]; !exists {
				state := &fileState{total: pr.TotalSize}
				fileProgress[pr.Filepath] = state
				// This total is for all files that *might* be downloaded or verified
				totalVerifiedSize += pr.TotalSize
				totalDownloadSize += pr.TotalSize
			}
			state := fileProgress[pr.Filepath]
			lastActiveFile = pr.Filepath

			switch pr.State {
			case hfg.ProgressStateVerifying:
				currentPhase = "Verifying"
				// Verification progress is absolute
				delta := pr.CurrentSize - state.verifiedBytes
				if delta > 0 {
					state.verifiedBytes += delta
					totalVerified += delta
				}
			case hfg.ProgressStateDownloading:
				if currentPhase != "Downloading" {
					// First download event, reset totals for download phase
					currentPhase = "Downloading"
					totalDownloaded = totalVerified // Start download progress from where verification left off
				}
				if downloadStartTime.IsZero() {
					downloadStartTime = time.Now()
					recentTime = time.Now()
				}
				delta := pr.CurrentSize
				state.downloadedBytes += delta
				totalDownloaded += delta
				recentBytes += delta
			case hfg.ProgressStateVerified:
				delta := state.total - state.verifiedBytes
				state.verifiedBytes += delta
				totalVerified += delta
			case hfg.ProgressStateSkipped:
				delta := pr.TotalSize - state.verifiedBytes
				state.verifiedBytes += delta
				totalVerified += delta
			}
		case <-ticker.C:
			// ---- RENDER LOGIC ----
			if linesPrinted > 0 {
				fmt.Fprint(out, strings.Repeat(moveUp, linesPrinted-1), clearLine)
			}
			width, _, _ := term.GetSize(fd)
			if width == 0 {
				width = 90 // Fallback width
			}

			if currentPhase == "Verifying" {
				spinnerIndex = (spinnerIndex + 1) % len(spinner)
				var filePercent float64
				if state, ok := fileProgress[lastActiveFile]; ok && state.total > 0 {
					filePercent = float64(state.verifiedBytes) * 100 / float64(state.total)
				}
				output := fmt.Sprintf("Verifying: [%c] %s (%.1f%%)", spinner[spinnerIndex], lastActiveFile, filePercent)
				fmt.Fprint(out, clearLine, output)
				linesPrinted = 1
			} else { // Downloading
				var overallPercent float64
				if totalDownloadSize > 0 {
					overallPercent = float64(totalDownloaded) * 100 / float64(totalDownloadSize)
				}
				elapsed := time.Since(downloadStartTime).Seconds()
				avgSpeed := float64(totalDownloaded) / elapsed
				var currentSpeed float64
				recentElapsed := time.Since(recentTime).Seconds()
				if recentElapsed > 0.5 {
					currentSpeed = float64(recentBytes) / recentElapsed
					recentBytes = 0
					recentTime = time.Now()
				}
				line1 := fmt.Sprintf("Overall: %.1f%% (%s/%s) | Avg: %s/s | Current: %s/s",
					overallPercent, formatBytes(totalDownloaded), formatBytes(totalDownloadSize), formatBytes(int64(avgSpeed)), formatBytes(int64(currentSpeed)))

				var line2 string
				if state, ok := fileProgress[lastActiveFile]; ok && state.total > 0 && state.downloadedBytes < state.total {
					filePercent := float64(state.downloadedBytes) * 100 / float64(state.total)
					line2 = fmt.Sprintf("File: %s (%.1f%%)", truncateString(lastActiveFile, width-20), filePercent)
				} else {
					line2 = "Finalizing..."
				}

				if len(line1)+len(line2) > width-5 {
					fmt.Fprint(out, clearLine, line1, "\n", clearLine, line2)
					linesPrinted = 2
				} else {
					output := fmt.Sprintf("%s | %s", line1, line2)
					fmt.Fprint(out, clearLine, output)
					linesPrinted = 1
				}
			}
		}
	}
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

func truncateString(s string, maxLen int) string {
	if s == "" {
		return ""
	}
	if len(s) <= maxLen {
		return s
	}
	return "..." + s[len(s)-maxLen+3:]
}
