package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	hfg "github.com/drgo/hfget"
)

const VERSION = "4.0.0"

func main() {
	log.SetFlags(0) // Remove timestamps from logger
	// --- 1. Flag Definitions ---
	var (
		isDatasetFlag  bool
		branch         string
		storage        string
		numConnections int
		token          string
		skipSHA        bool
		maxRetries     int
		retryInterval  time.Duration
		quiet          bool
		force          bool
	)

	flag.BoolVar(&isDatasetFlag, "d", false, "Specify that the repo is a dataset")
	flag.StringVar(&branch, "b", "main", "Branch of the model or dataset")
	flag.StringVar(&storage, "s", "./", "Storage path for downloads")
	flag.IntVar(&numConnections, "c", 5, "Number of concurrent connections")
	flag.StringVar(&token, "t", os.Getenv("HF_TOKEN"), "HuggingFace Auth Token (or from HF_TOKEN env var)")
	flag.BoolVar(&skipSHA, "k", false, "Skip SHA256 hash check")
	flag.IntVar(&maxRetries, "max-retries", 3, "Maximum number of retries")
	flag.DurationVar(&retryInterval, "retry-interval", 5*time.Second, "Interval between retries")
	flag.BoolVar(&quiet, "q", false, "Quiet mode (suppress progress display and prompts)")
	flag.BoolVar(&force, "f", false, "Force download (synonym for --quiet)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] model_or_dataset_name\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "Example: hfget TheBloke/Llama-2-7B-GGUF")
		fmt.Fprintln(os.Stderr, "Options:")
		flag.PrintDefaults()
	}

	flag.Parse()

	if force {
		quiet = true
	}

	// --- 2. Argument and Repo Handling ---
	if flag.NArg() < 1 {
		handleError("a model or dataset name argument is required.")
	}
	repoName := flag.Arg(0)

	// --- 3. Planning Phase ---
	opts := []hfg.Option{
		hfg.WithBranch(branch),
		hfg.WithDestination(storage),
		hfg.WithConnections(numConnections),
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

	downloader := hfg.New(repoName, opts...)
	log.Println("Analyzing repository...")
	plan, err := downloader.GetDownloadPlan(context.Background())
	if err != nil {
		handleError(fmt.Sprintf("could not analyze repository: %v", err))
	}

	if len(plan.FilesToDownload) == 0 {
		log.Println("All model files are already present and valid. Nothing to download.")
		os.Exit(0)
	}

	// --- 4. Confirmation Prompt ---
	if !quiet {
		fmt.Println("----------------------------------------------------")
		fmt.Printf("Repository: %s\n", plan.Repo.ID)
		fmt.Printf("Last Modified: %s\n", plan.Repo.LastModified.Format(time.RFC1123))
		fmt.Println("----------------------------------------------------")
		fmt.Println("Files to be downloaded:")
		for _, file := range plan.FilesToDownload {
			fmt.Printf("  - %-60s (%s)\n", file.Path, formatBytes(file.Size))
		}
		fmt.Println("----------------------------------------------------")
		fmt.Printf("Total download size: %s\n", formatBytes(plan.TotalSize))
		fmt.Print("Proceed with download? [y/N]: ")

		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		if strings.TrimSpace(strings.ToLower(input)) != "y" {
			log.Println("Download cancelled by user.")
			os.Exit(0)
		}
	}

	// --- 5. Execution Phase ---
	var wg sync.WaitGroup
	if !quiet {
		progressChan := make(chan hfg.Progress)
		// Add progress channel option to existing options
		opts = append(opts, hfg.WithProgressChannel(progressChan))
		downloader = hfg.New(repoName, opts...)

		wg.Add(1)
		go func() {
			defer wg.Done()
			displayProgressStdLib(progressChan)
			close(progressChan) // Ensure channel is closed when goroutine exits
		}()
	}

	log.Println("Starting download...")
	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			log.Printf("Attempt %d of %d after error: %v", i+1, maxRetries, err)
			time.Sleep(retryInterval)
		}
		err = downloader.ExecutePlan(context.Background(), plan)
		if err == nil {
			break
		}
		if !isTransientError(err) {
			handleError(fmt.Sprintf("a fatal error occurred: %v", err))
		}
	}

	if !quiet {
		// This wait ensures the progress display finishes drawing before the final log messages.
		wg.Wait()
		fmt.Println()
	}

	if err != nil {
		handleError(fmt.Sprintf("failed to download after %d attempts: %v", maxRetries, err))
	}

	log.Printf("Download of %s completed successfully.", repoName)
}

// handleError prints a concise error and exits.
func handleError(msg string) {
	fmt.Fprintf(os.Stderr, "Error: %s\n", msg)
	fmt.Fprintln(os.Stderr, "Run with -h for usage information.")
	os.Exit(1)
}

func isTransientError(err error) bool {
	if errors.Is(err, hfg.ErrAuthentication) || errors.Is(err, hfg.ErrForbidden) || errors.Is(err, hfg.ErrNotFound) {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

type fileState struct {
	total, downloaded int64
}

func displayProgressStdLib(progressChan <-chan hfg.Progress) {
	fileProgress := make(map[string]*fileState)
	var totalSize, totalDownloaded int64
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var lastActiveFile string

	updateDisplay := func() {
		var currentFileDisplay string
		if state, ok := fileProgress[lastActiveFile]; ok && state.total > 0 {
			filePercent := float64(state.downloaded) * 100 / float64(state.total)
			currentFileDisplay = fmt.Sprintf(" | %s: %.1f%%", truncateString(lastActiveFile, 20), filePercent)
		}
		totalPercent := 0.0
		if totalSize > 0 {
			totalPercent = float64(totalDownloaded) * 100 / float64(totalSize)
		}
		output := fmt.Sprintf("Total: %.1f%% (%s / %s)%s", totalPercent, formatBytes(totalDownloaded), formatBytes(totalSize), currentFileDisplay)
		fmt.Printf("\r%-80s", output)
	}

	for {
		select {
		case pr, ok := <-progressChan:
			if !ok {
				updateDisplay()
				return
			}
			if _, exists := fileProgress[pr.Filepath]; !exists {
				fileProgress[pr.Filepath] = &fileState{total: pr.TotalSize}
				totalSize += pr.TotalSize
			}
			state := fileProgress[pr.Filepath]
			lastActiveFile = pr.Filepath
			delta := pr.CurrentSize - state.downloaded
			state.downloaded += delta
			totalDownloaded += delta
			if pr.State == hfg.ProgressStateSkipped {
				totalDownloaded += (pr.TotalSize - state.downloaded)
				state.downloaded = pr.TotalSize
			}
		case <-ticker.C:
			updateDisplay()
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
	if len(s) <= maxLen {
		return s
	}
	return "..." + s[len(s)-maxLen+3:]
}
