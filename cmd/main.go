package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	hfg "github.com/drgo/hfget"
	flag "github.com/spf13/pflag"
	"golang.org/x/term"
)

var VERSION = ""

const (
	moveUp            = "\033[A"
	clearLine         = "\r\033[2K"
	interruptExitCode = 130
)

var errInterrupted = errors.New("interrupted")

// legacyLongNames maps the single-letter long names advertised by older
// --help output onto the current names so --d, --c, etc. keep working.
var legacyLongNames = map[string]string{
	"b": "branch",
	"d": "dest",
	"c": "connections",
	"t": "token",
	"q": "quiet",
	"f": "force",
	"v": "verbose",
}

func normalizeFlag(_ *flag.FlagSet, name string) flag.NormalizedName {
	if canonical, ok := legacyLongNames[name]; ok {
		return flag.NormalizedName(canonical)
	}
	return flag.NormalizedName(name)
}

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
	in            io.Reader
	out           io.Writer
	err           io.Writer
	isTerminal    bool
	terminalFd    int
	ctx           context.Context
	exit          func(int)
	newDownloader func(repoName string, opts ...hfg.Option) downloader
}

func main() {
	fd := int(os.Stderr.Fd())
	app := &cliApp{
		in:         os.Stdin,
		out:        os.Stdout,
		err:        os.Stderr,
		isTerminal: term.IsTerminal(fd),
		terminalFd: fd,
		newDownloader: func(repoName string, opts ...hfg.Option) downloader {
			return &realDownloader{Downloader: hfg.New(repoName, opts...)}
		},
	}
	ctx, stop := app.withInterrupt(context.Background())
	defer stop()
	app.ctx = ctx
	if err := app.run(os.Args[1:]); err != nil {
		if errors.Is(err, errInterrupted) {
			os.Exit(interruptExitCode)
		}
		log.New(app.err, "", 0).Printf("Error:\n%v", err)
		os.Exit(1)
	}
}

func (app *cliApp) runContext() context.Context {
	if app.ctx != nil {
		return app.ctx
	}
	return context.Background()
}

func (app *cliApp) run(args []string) error {
	log.SetOutput(app.err)
	log.SetFlags(0)

	stdinReader := bufio.NewReader(os.Stdin)

	var (
		isDatasetFlag   bool
		branch          string
		dest            string
		numConnections  int
		token           string
		skipChecksum    bool
		maxRetries      int
		retryInterval   time.Duration
		quiet           bool
		yes             bool
		force           bool
		useTree         bool
		includePatterns string
		excludePatterns string
		showVersion     bool
		verbose         bool
	)

	fs := flag.NewFlagSet("hfget", flag.ContinueOnError)
	fs.SetOutput(app.err)
	fs.SetNormalizeFunc(normalizeFlag)

	fs.BoolVar(&isDatasetFlag, "dataset", false, "Specify that the repo is a dataset")
	fs.StringVarP(&branch, "branch", "b", envOrDefault("HFGET_BRANCH", "main"), "Branch of the model or dataset ($HFGET_BRANCH)")
	fs.StringVarP(&dest, "dest", "d", envOrDefault("HFGET_DEST", "./"), "Destination base path for downloads ($HFGET_DEST)")
	defaultConnections, _ := strconv.Atoi(envOrDefault("HFGET_CONCURRENT_CONNECTIONS", "5"))
	fs.IntVarP(&numConnections, "connections", "c", defaultConnections, "Number of concurrent connections ($HFGET_CONCURRENT_CONNECTIONS)")
	fs.StringVarP(&token, "token", "t", envOrDefault("HFGET_TOKEN", ""), "HuggingFace Auth Token ($HFGET_TOKEN)")
	defaultSkipChecksum, _ := strconv.ParseBool(envOrDefault("HFGET_SKIP_CHECKSUM", "false"))
	fs.BoolVar(&skipChecksum, "skip-checksum", defaultSkipChecksum, "Skip SHA256 checksum verification ($HFGET_SKIP_CHECKSUM)")
	fs.IntVar(&maxRetries, "max-retries", 3, "Maximum number of retries")
	fs.DurationVar(&retryInterval, "retry-interval", 5*time.Second, "Interval between retries")
	fs.BoolVarP(&quiet, "quiet", "q", false, "Quiet mode (suppress progress display and prompts)")
	fs.BoolVarP(&yes, "yes", "y", false, "Skip the proceed prompt; does not force a re-download (use -f). Keeps progress display")
	fs.BoolVarP(&force, "force", "f", false, "Force re-download of all files, implies quiet mode")
	fs.BoolVar(&useTree, "tree", false, "Save under nested repo/model_name directories (e.g. 'org/model' instead of 'org_model')")
	fs.StringVar(&includePatterns, "include", "", "Comma-separated glob patterns for files to download, e.g., '*Q8_0*'")
	fs.StringVar(&excludePatterns, "exclude", "", "Comma-separated glob patterns for files to exclude")
	fs.BoolVar(&showVersion, "version", false, "Show version information")
	fs.BoolVarP(&verbose, "verbose", "v", false, "Enable verbose diagnostic logging to stderr")

	fs.Usage = func() {
		fmt.Fprintf(app.err, "Usage: %s [options] model_or_dataset_name\n", os.Args[0])
		fmt.Fprintln(app.err, "Example: hfget TheBloke/Llama-2-7B-GGUF --include \"*.gguf\"")
		fmt.Fprintln(app.err, "Options:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if showVersion {
		fmt.Fprintf(app.out, "hfget version %s\n", VERSION)
		return nil
	}

	if fs.NArg() < 1 {
		return errors.New("a model or dataset name argument is required")
	}
	repoName := fs.Arg(0)

	// Non-interactive terminals and --force suppress prompts/progress.
	// --yes only auto-confirms prompts; progress still shows unless --quiet.
	if !app.isTerminal || force {
		quiet = true
	}

	opts := []hfg.Option{
		hfg.WithBranch(branch), hfg.WithDestination(dest), hfg.WithConnections(numConnections),
	}
	if isDatasetFlag {
		opts = append(opts, hfg.AsDataset())
	}
	if token != "" {
		opts = append(opts, hfg.WithAuthToken(token))
	}
	if skipChecksum {
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
	ctx := app.runContext()
	downer := app.newDownloader(repoName, opts...)
	fmt.Fprintln(app.err, "Fetching repository information...")
	repoInfo, err := downer.FetchRepoInfo(ctx)
	if err != nil {
		if cerr := app.ifCancelled(err); cerr != nil {
			return cerr
		}
		return fmt.Errorf("could not fetch repository info: %w", err)
	}

	effectiveDest := hfg.ModelDir(dest, repoInfo.ID, useTree)

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
		downer = app.newDownloader(repoName, optsWithProgress...)

		wg.Go(func() {
			analysisDisplayProgress(app.err, progressChan, app.terminalFd, totalAnalysisSize, effectiveDest)
		})
	}

	plan, err := downer.BuildPlan(ctx, repoInfo)
	if !quiet {
		close(progressChan)
		wg.Wait()
	}
	if err != nil {
		if cerr := app.ifCancelled(err); cerr != nil {
			return cerr
		}
		return fmt.Errorf("could not build download plan: %w", err)
	}

	if len(plan.FilesToDownload) == 0 {
		if len(plan.FilesToSkip) > 0 {
			log.Printf("%d files are already present and valid (Total Size: %s).", len(plan.FilesToSkip), formatBytes(plan.TotalSkipSize))
		}
		log.Println("Nothing to download.")

		// Interactive only: offer an optional force re-download. -y / -q / non-TTY
		// treat "nothing to do" as success (use -f to force re-download unattended).
		if !force && !quiet && !yes {
			ok, cerr := app.confirm(ctx, stdinReader, "Would you like to force a re-download anyway? [y/N]: ")
			if cerr != nil {
				if err := app.ifCancelled(cerr); err != nil {
					return err
				}
				return nil
			}
			if ok {
				log.Println("Forcing re-download as requested...")
				for _, skippedFile := range plan.FilesToSkip {
					plan.FilesToDownload = append(plan.FilesToDownload, hfg.FileDownload{File: skippedFile.File, Reason: "forced re-download"})
				}
				plan.TotalDownloadSize += plan.TotalSkipSize
				plan.FilesToSkip = nil
				plan.TotalSkipSize = 0
			} else {
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
		fmt.Fprintf(app.err, "Destination:   %s\n", effectiveDest)
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
		if avail, spaceErr := hfg.AvailableSpace(effectiveDest); spaceErr == nil {
			fmt.Fprintf(app.err, "Available space:     %s\n", formatBytes(avail))
		}
	}

	if err := hfg.EnsureWritableSpace(effectiveDest, plan, numConnections); err != nil {
		return err
	}

	if !quiet {
		if !yes {
			ok, cerr := app.confirm(ctx, stdinReader, "Proceed with download? [y/N]: ")
			if cerr != nil {
				if err := app.ifCancelled(cerr); err != nil {
					return err
				}
				return nil
			}
			if !ok {
				return nil
			}
		} else {
			fmt.Fprintln(app.err, "Proceeding with download (-y).")
		}
	}

	if !quiet {
		progressChan = make(chan hfg.Progress, numConnections*2)
		optsWithProgress := append(opts, hfg.WithProgressChannel(progressChan))
		downer = app.newDownloader(repoName, optsWithProgress...)

		wg.Go(func() {
			downloadDisplayProgress(ctx, app.err, progressChan, app.terminalFd, plan)
		})
	}

	var lastErr error
	for i := 0; i < maxRetries; i++ {
		if err := ctx.Err(); err != nil {
			lastErr = err
			break
		}
		if i > 0 {
			log.Printf("Retrying after transient error (attempt %d/%d)...", i+1, maxRetries)
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
			case <-time.After(retryInterval):
			}
			if ctx.Err() != nil {
				lastErr = ctx.Err()
				break
			}
		}
		lastErr = downer.ExecutePlan(ctx, plan)
		if lastErr == nil {
			break
		}
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		if !isTransientError(lastErr) {
			break
		}
	}

	if !quiet {
		close(progressChan)
		wg.Wait()
	}

	if lastErr != nil {
		if cerr := app.ifCancelled(lastErr); cerr != nil {
			return cerr
		}
		return lastErr
	}

	return nil
}

type fileProgressState struct {
	processedBytes int64
	totalSize      int64
	state          hfg.ProgressState
}

func analysisDisplayProgress(out io.Writer, progressChan <-chan hfg.Progress, fd int, totalAnalysisSize int64, dest string) {
	fileStates := make(map[string]*fileProgressState)
	var lastActiveFile string
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case pr, ok := <-progressChan:
			if !ok {
				fmt.Fprint(out, clearLine)
				fmt.Fprintln(out, dest)
				return
			}
			lastActiveFile = pr.Filepath

			state, exists := fileStates[pr.Filepath]
			if !exists {
				state = &fileProgressState{totalSize: pr.TotalSize}
				fileStates[pr.Filepath] = state
			}

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

func downloadDisplayProgress(ctx context.Context, out io.Writer, progressChan <-chan hfg.Progress, fd int, plan *hfg.DownloadPlan) {
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
				if ctx.Err() != nil {
					return
				}
				fmt.Printf("Overall: 100.0%% (%s/%s) | Complete.\n\n", formatBytes(totalDownloadSize), formatBytes(totalDownloadSize))
				return
			}
			state, exists := fileStates[pr.Filepath]
			if !exists {
				continue
			}
			state.state = pr.State

			switch pr.State {
			case hfg.ProgressStateDownloading:
				if pr.CurrentSize > state.processedBytes {
					delta := pr.CurrentSize - state.processedBytes
					totalDownloaded += delta
					recentBytes += delta
				}
				state.processedBytes = pr.CurrentSize

			case hfg.ProgressStateComplete, hfg.ProgressStateVerified:
				if state.processedBytes < state.totalSize {
					delta := state.totalSize - state.processedBytes
					totalDownloaded += delta
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

func (app *cliApp) withInterrupt(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	// Size 1: extra SIGINTs while we are not receiving are dropped instead of
	// being queued as a fake "second Ctrl+C".
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	stopCh := make(chan struct{})
	go watchSignals(cancel, sigCh, stopCh, app.err, app.exit)
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			signal.Stop(sigCh)
			close(stopCh)
			cancel()
		})
	}
}

func watchSignals(cancel context.CancelFunc, sig <-chan os.Signal, stop <-chan struct{}, out io.Writer, exit func(int)) {
	if exit == nil {
		exit = os.Exit
	}
	select {
	case <-stop:
		return
	case <-sig:
	}
	fmt.Fprintln(out, "\nInterrupt received. Press Ctrl+C again to force quit.")
	cancel()
	// One keypress can deliver a duplicate SIGINT (terminal/process-group bounce)
	// while cancel() runs. Drain it so the first Ctrl+C is not treated as force-quit.
	drainSignals(sig)
	select {
	case <-stop:
		return
	case <-sig:
		fmt.Fprintln(out, "Forced quit.")
		exit(interruptExitCode)
	}
}

func drainSignals(sig <-chan os.Signal) {
	for {
		select {
		case <-sig:
		default:
			return
		}
	}
}

func (app *cliApp) confirm(ctx context.Context, r *bufio.Reader, prompt string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	fmt.Fprint(app.err, prompt)
	type reply struct {
		line string
		err  error
	}
	ch := make(chan reply, 1)
	go func() {
		line, err := r.ReadString('\n')
		ch <- reply{line, err}
	}()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case res := <-ch:
		// Ctrl+C can unblock stdin in the same moment the signal cancels ctx.
		// Prefer the interrupt so a single SIGINT is not treated as "no" / EOF.
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if res.err != nil {
			return false, res.err
		}
		return strings.TrimSpace(strings.ToLower(res.line)) == "y", nil
	}
}

func (app *cliApp) ifCancelled(err error) error {
	if err != nil && errors.Is(err, context.Canceled) {
		fmt.Fprintln(app.err, "Cancelled.")
		return errInterrupted
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
	if err == nil {
		return false
	}
	if errors.Is(err, hfg.ErrAuthentication) || errors.Is(err, hfg.ErrForbidden) || errors.Is(err, hfg.ErrNotFound) || errors.Is(err, hfg.ErrInsufficientSpace) {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}

	if netErr, ok := errors.AsType[net.Error](err); ok {
		return netErr.Timeout()
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
	rs := []rune(s)
	if len(rs) <= maxLen {
		return s
	}
	return "..." + string(rs[len(rs)-maxLen+3:])
}
