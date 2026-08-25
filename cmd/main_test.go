package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hfg "github.com/drgo/hfget"
	"github.com/drgo/hfget/testutils"
)

type mockDownloader struct {
	repoInfoToReturn *hfg.RepoInfo
	planToReturn     *hfg.DownloadPlan

	// Specific errors for each phase
	fetchErr   error
	buildErr   error
	executeErr error

	// Track calls
	fetchRepoInfoCalls int
	buildPlanCalls     int
	executePlanCalls   int

	// For retry tests
	executePlanFailures int
}

func (m *mockDownloader) FetchRepoInfo(ctx context.Context) (*hfg.RepoInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.fetchRepoInfoCalls++
	if m.repoInfoToReturn == nil {
		return &hfg.RepoInfo{ID: "test/repo"}, m.fetchErr
	}
	return m.repoInfoToReturn, m.fetchErr
}

func (m *mockDownloader) BuildPlan(ctx context.Context, repoInfo *hfg.RepoInfo) (*hfg.DownloadPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.buildPlanCalls++
	if m.planToReturn == nil {
		return &hfg.DownloadPlan{Repo: repoInfo}, m.buildErr
	}
	return m.planToReturn, m.buildErr
}

func (m *mockDownloader) ExecutePlan(ctx context.Context, plan *hfg.DownloadPlan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.executePlanCalls++
	if m.executePlanCalls <= m.executePlanFailures {
		return m.executeErr
	}
	return nil
}

// mockStdin is a helper to simulate user input for interactive prompts.
func mockStdin(t *testing.T, input string) (restore func()) {
	t.Helper()
	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Failed to create pipe for stdin mock: %v", err)
	}
	os.Stdin = r
	go func() {
		defer w.Close()
		_, _ = io.WriteString(w, input)
	}()
	return func() {
		os.Stdin = oldStdin
	}
}

func TestCLI(t *testing.T) {
	defaultPlan := &hfg.DownloadPlan{
		Repo: &hfg.RepoInfo{ID: "test/repo", LastModified: time.Now()},
		FilesToDownload: []hfg.FileDownload{
			{File: hfg.HFFile{Path: "file1.txt", Size: 1024}},
			{File: hfg.HFFile{Path: "file2.bin", Size: 2048}},
		},
		TotalDownloadSize: 3072,
	}
	defaultRepoInfo := &hfg.RepoInfo{
		ID: "test/repo",
	}

	t.Run("Missing repository argument", func(t *testing.T) {
		require := testutils.NewRequire(t)
		app := &cliApp{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
		err := app.run([]string{})
		require.Error(err, "Expected an error for missing argument, but got none")
		require.True(strings.Contains(err.Error(), "argument is required"), "Expected error message to contain 'argument is required', got: %v", err)
	})

	// Regression: unknown short flags used to be swallowed (return nil), so
	// e.g. an unsupported -y would exit 0 immediately with no download.
	t.Run("Unknown shorthand flag returns error", func(t *testing.T) {
		require := testutils.NewRequire(t)
		app := &cliApp{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
		err := app.run([]string{"-z", "test/repo"})
		require.Error(err, "Expected an error for unknown shorthand flag, but got none")
	})

	t.Run("Force flag implies quiet and skips prompt", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		mock := &mockDownloader{
			repoInfoToReturn: defaultRepoInfo,
			planToReturn:     defaultPlan,
		}
		app := &cliApp{
			out:           out,
			err:           errOut,
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		// Use -f for force
		err := app.run([]string{"-f", "test/repo"})
		require.NoError(err, "Expected no error for forced download")
		// There should be no interactive prompt in the output
		assert.False(strings.Contains(errOut.String(), "Proceed with download? [y/N]:"), "Expected force flag to skip the confirmation prompt")
		assert.True(mock.executePlanCalls == 1, "Expected ExecutePlan to be called once, but was called %d times", mock.executePlanCalls)
	})

	t.Run("Yes flag skips confirmation but keeps progress path", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		mock := &mockDownloader{
			repoInfoToReturn: defaultRepoInfo,
			planToReturn:     defaultPlan,
		}
		app := &cliApp{
			out:           out,
			err:           errOut,
			isTerminal:    true, // terminal so quiet is not auto-enabled
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		err := app.run([]string{"-y", "test/repo"})
		require.NoError(err, "Expected no error with -y")
		assert.False(strings.Contains(errOut.String(), "Proceed with download? [y/N]:"), "Expected -y to skip the confirmation prompt")
		assert.True(strings.Contains(errOut.String(), "Proceeding with download (-y)."), "Expected -y proceed message")
		assert.True(mock.executePlanCalls == 1, "Expected ExecutePlan to be called once, but was called %d times", mock.executePlanCalls)
	})

	t.Run("Yes flag exits when nothing to download without re-download", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		errOut := &bytes.Buffer{}
		emptyPlan := &hfg.DownloadPlan{
			Repo:        defaultRepoInfo,
			FilesToSkip: []hfg.FileSkip{{File: hfg.HFFile{Path: "file1.txt"}}},
		}
		mock := &mockDownloader{
			repoInfoToReturn: defaultRepoInfo,
			planToReturn:     emptyPlan,
		}
		app := &cliApp{
			out:           &bytes.Buffer{},
			err:           errOut,
			isTerminal:    true,
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		err := app.run([]string{"-y", "test/repo"})
		require.NoError(err, "Expected no error when nothing to download with -y")
		assert.True(strings.Contains(errOut.String(), "Nothing to download."), "Expected nothing-to-download message")
		assert.False(strings.Contains(errOut.String(), "Would you like to force a re-download"), "Expected -y not to offer re-download")
		assert.True(mock.executePlanCalls == 0, "Expected ExecutePlan not to be called, but was called %d times", mock.executePlanCalls)
	})

	t.Run("No files to download, exits gracefully", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		out := &bytes.Buffer{}
		emptyPlan := &hfg.DownloadPlan{FilesToDownload: []hfg.FileDownload{}}
		mock := &mockDownloader{
			repoInfoToReturn: defaultRepoInfo,
			planToReturn:     emptyPlan,
		}
		app := &cliApp{
			out:           out,
			err:           &bytes.Buffer{},
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		err := app.run([]string{"test/repo"})
		require.NoError(err, "Expected no error when no files need downloading, got: %v", err)
		assert.True(strings.Contains(app.err.(*bytes.Buffer).String(), "Nothing to download."), "Expected to see the 'Nothing to download' message")
		assert.True(mock.executePlanCalls == 0, "Expected ExecutePlan to not be called, but was called %d times", mock.executePlanCalls)
	})

	t.Run("Interactive prompt to re-download", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		restore := mockStdin(t, "y\ny\n") // Simulate "y" for re-download and "y" for confirmation
		defer restore()

		out := &bytes.Buffer{}
		errOut := &bytes.Buffer{}
		// Plan is initially empty, but has skippable files
		emptyPlan := &hfg.DownloadPlan{
			Repo:        defaultRepoInfo,
			FilesToSkip: []hfg.FileSkip{{File: hfg.HFFile{Path: "file1.txt"}}},
		}
		mock := &mockDownloader{
			repoInfoToReturn: defaultRepoInfo,
			planToReturn:     emptyPlan,
		}
		// --- FIX 2: Set isTerminal to true for this test ---
		app := &cliApp{
			out:           out,
			err:           errOut,
			isTerminal:    true, // This ensures prompts are shown
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		err := app.run([]string{"test/repo"})
		require.NoError(err, "Expected no error after re-download confirmation, got: %v", err)

		assert.True(strings.Contains(errOut.String(), "Would you like to force a re-download anyway?"), "Expected the interactive re-download prompt to be shown")
		// The plan is modified in-place, so ExecutePlan will be called.
		assert.True(mock.executePlanCalls == 1, "Expected ExecutePlan to be called once, but was called %d times", mock.executePlanCalls)
	})

	t.Run("Retry on transient error", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		// --- FIX 1: Set executeErr instead of the general errToReturn ---
		mock := &mockDownloader{
			repoInfoToReturn:    defaultRepoInfo,
			planToReturn:        defaultPlan,
			executeErr:          os.ErrDeadlineExceeded, // A generic transient error
			executePlanFailures: 1,                      // Fail on the first attempt
		}
		app := &cliApp{
			out:           &bytes.Buffer{},
			err:           &bytes.Buffer{},
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		// Use a very short retry interval for the test and force flag to skip prompts
		err := app.run([]string{"--retry-interval", "1ms", "-f", "test/repo"})
		require.NoError(err, "Expected no final error after retry, got: %v", err)

		assert.True(mock.executePlanCalls == 2, "Expected ExecutePlan to be called 2 times, but was called %d times", mock.executePlanCalls)
		assert.True(strings.Contains(app.err.(*bytes.Buffer).String(), "Retrying after transient error"), "Expected to see the retry attempt message in the logs")
	})

	t.Run("No retry on fatal error", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		// --- FIX 1: Set executeErr instead of the general errToReturn ---
		mock := &mockDownloader{
			repoInfoToReturn:    defaultRepoInfo,
			planToReturn:        defaultPlan,
			executeErr:          hfg.ErrAuthentication, // A fatal error
			executePlanFailures: 1,
		}
		app := &cliApp{
			out:           &bytes.Buffer{},
			err:           &bytes.Buffer{},
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		err := app.run([]string{"-f", "test/repo"})
		require.Error(err, "Expected a fatal error, but got none")

		assert.True(mock.executePlanCalls == 1, "Expected ExecutePlan to be called only once, but was called %d times", mock.executePlanCalls)
	})

	t.Run("Destination uses canonical repo ID not CLI argument", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		errOut := &bytes.Buffer{}
		canonical := &hfg.RepoInfo{ID: "google-bert/bert-base-uncased", LastModified: time.Now()}
		plan := &hfg.DownloadPlan{
			Repo: canonical,
			FilesToDownload: []hfg.FileDownload{
				{File: hfg.HFFile{Path: "config.json", Size: 100}, Reason: "missing"},
			},
			TotalDownloadSize: 100,
		}
		mock := &mockDownloader{repoInfoToReturn: canonical, planToReturn: plan}
		app := &cliApp{
			out:           &bytes.Buffer{},
			err:           errOut,
			isTerminal:    true,
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		err := app.run([]string{"-y", "-d", "./models", "bert-base-uncased"})
		require.NoError(err, "Expected no error for canonical-id destination")
		want := "Destination:   " + filepath.Join("models", "google-bert_bert-base-uncased")
		assert.True(strings.Contains(errOut.String(), want), "Expected destination %q in output, got: %s", want, errOut.String())
		assert.False(strings.Contains(errOut.String(), "Destination:   "+filepath.Join("models", "bert-base-uncased")),
			"Destination should not use the unresolved CLI repo name")
	})

	t.Run("Destination with --tree uses nested canonical ID", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		errOut := &bytes.Buffer{}
		canonical := &hfg.RepoInfo{ID: "google-bert/bert-base-uncased", LastModified: time.Now()}
		plan := &hfg.DownloadPlan{
			Repo: canonical,
			FilesToDownload: []hfg.FileDownload{
				{File: hfg.HFFile{Path: "config.json", Size: 100}, Reason: "missing"},
			},
			TotalDownloadSize: 100,
		}
		mock := &mockDownloader{repoInfoToReturn: canonical, planToReturn: plan}
		app := &cliApp{
			out:           &bytes.Buffer{},
			err:           errOut,
			isTerminal:    true,
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		err := app.run([]string{"-y", "--tree", "-d", "./models", "bert-base-uncased"})
		require.NoError(err, "Expected no error for tree destination")
		want := "Destination:   " + filepath.Join("models", "google-bert", "bert-base-uncased")
		assert.True(strings.Contains(errOut.String(), want), "Expected destination %q in output, got: %s", want, errOut.String())
	})

	t.Run("Legacy single-letter long flags still parse", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		mock := &mockDownloader{
			repoInfoToReturn: defaultRepoInfo,
			planToReturn:     defaultPlan,
		}
		app := &cliApp{
			out:           &bytes.Buffer{},
			err:           &bytes.Buffer{},
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		err := app.run([]string{"--d", "./models", "--c", "2", "--q", "test/repo"})
		require.NoError(err, "Expected legacy --d/--c/--q to parse, got: %v", err)
		assert.True(mock.executePlanCalls == 1, "Expected ExecutePlan to be called once, but was called %d times", mock.executePlanCalls)
	})

	t.Run("Shows available space in plan summary", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		errOut := &bytes.Buffer{}
		mock := &mockDownloader{
			repoInfoToReturn: defaultRepoInfo,
			planToReturn:     defaultPlan,
		}
		app := &cliApp{
			out:           &bytes.Buffer{},
			err:           errOut,
			isTerminal:    true,
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		err := app.run([]string{"-y", "test/repo"})
		require.NoError(err, "Expected no error when space is available")
		assert.True(strings.Contains(errOut.String(), "Available space:"), "Expected available space in summary, got: %s", errOut.String())
		assert.True(mock.executePlanCalls == 1, "Expected ExecutePlan to be called once, but was called %d times", mock.executePlanCalls)
	})

	t.Run("Insufficient disk space aborts before execute", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		errOut := &bytes.Buffer{}
		hugePlan := &hfg.DownloadPlan{
			Repo: defaultRepoInfo,
			FilesToDownload: []hfg.FileDownload{
				{File: hfg.HFFile{Path: "huge.bin", Size: math.MaxInt64 / 2}},
			},
			TotalDownloadSize: math.MaxInt64 / 2,
		}
		mock := &mockDownloader{
			repoInfoToReturn: defaultRepoInfo,
			planToReturn:     hugePlan,
		}
		app := &cliApp{
			out:           &bytes.Buffer{},
			err:           errOut,
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		err := app.run([]string{"-f", "test/repo"})
		require.Error(err, "Expected insufficient space error")
		assert.True(errors.Is(err, hfg.ErrInsufficientSpace), "Expected ErrInsufficientSpace, got: %v", err)
		assert.True(mock.executePlanCalls == 0, "Expected ExecutePlan not to run, but was called %d times", mock.executePlanCalls)
	})

	t.Run("Cancelled context stops before execute", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		errOut := &bytes.Buffer{}
		mock := &mockDownloader{
			repoInfoToReturn: defaultRepoInfo,
			planToReturn:     defaultPlan,
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		app := &cliApp{
			out:           &bytes.Buffer{},
			err:           errOut,
			ctx:           ctx,
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		err := app.run([]string{"-f", "test/repo"})
		require.Error(err, "Expected an interrupt error")
		assert.True(errors.Is(err, errInterrupted), "Expected errInterrupted, got: %v", err)
		assert.True(strings.Contains(errOut.String(), "Cancelled."), "Expected Cancelled message, got: %s", errOut.String())
		assert.True(mock.fetchRepoInfoCalls == 0, "Expected FetchRepoInfo not to run, but was called %d times", mock.fetchRepoInfoCalls)
		assert.True(mock.executePlanCalls == 0, "Expected ExecutePlan not to run, but was called %d times", mock.executePlanCalls)
	})
}

func TestConfirmInterruptPrefersContext(t *testing.T) {
	require := testutils.NewRequire(t)
	assert := testutils.NewAssert(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()

	app := &cliApp{err: io.Discard}
	errCh := make(chan error, 1)
	go func() {
		_, err := app.confirm(ctx, bufio.NewReader(pr), "Proceed? [y/N]: ")
		errCh <- err
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	_, _ = pw.Write([]byte("n\n"))

	select {
	case err := <-errCh:
		require.Error(err, "expected confirm to fail on interrupt")
		assert.True(errors.Is(err, context.Canceled), "expected context.Canceled, got: %v", err)
	case <-time.After(time.Second):
		t.Fatal("confirm did not return after cancel")
	}
}

func TestWatchSignals(t *testing.T) {
	t.Run("first signal cancels without exiting", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sig := make(chan os.Signal, 2)
		stop := make(chan struct{})
		errOut := &bytes.Buffer{}
		exited := make(chan int, 1)

		go watchSignals(cancel, sig, stop, errOut, func(code int) { exited <- code })
		sig <- os.Interrupt

		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("first signal did not cancel the context")
		}
		require.True(strings.Contains(errOut.String(), "Interrupt received"), "Expected first-interrupt message, got: %s", errOut.String())
		assert.True(strings.Contains(errOut.String(), "Press Ctrl+C again to force quit"), "Expected force-quit hint, got: %s", errOut.String())

		select {
		case code := <-exited:
			t.Fatalf("first signal should not force-exit, got code %d", code)
		case <-time.After(30 * time.Millisecond):
		}
		close(stop)
	})

	t.Run("queued duplicate of first signal does not force-exit", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sig := make(chan os.Signal, 2)
		stop := make(chan struct{})
		errOut := &bytes.Buffer{}
		exited := make(chan int, 1)

		sig <- os.Interrupt
		sig <- os.Interrupt
		go watchSignals(cancel, sig, stop, errOut, func(code int) { exited <- code })

		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("first signal did not cancel the context")
		}
		require.True(strings.Contains(errOut.String(), "Interrupt received"), "Expected first-interrupt message, got: %s", errOut.String())

		select {
		case code := <-exited:
			t.Fatalf("duplicate SIGINT from one keypress should not force-exit, got code %d", code)
		case <-time.After(50 * time.Millisecond):
		}
		assert.False(strings.Contains(errOut.String(), "Forced quit."), "duplicate should not print forced-quit, got: %s", errOut.String())
		close(stop)
	})

	t.Run("second signal force exits", func(t *testing.T) {
		require := testutils.NewRequire(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sig := make(chan os.Signal, 2)
		stop := make(chan struct{})
		defer close(stop)
		errOut := &bytes.Buffer{}
		exited := make(chan int, 1)

		go watchSignals(cancel, sig, stop, errOut, func(code int) { exited <- code })
		sig <- os.Interrupt
		<-ctx.Done()
		// Let drainSignals drop any bounce from the first press before the
		// real second Ctrl+C.
		time.Sleep(20 * time.Millisecond)
		sig <- os.Interrupt

		select {
		case code := <-exited:
			require.True(code == interruptExitCode, "Expected exit code %d, got %d", interruptExitCode, code)
		case <-time.After(time.Second):
			t.Fatal("second signal did not force-exit")
		}
		require.True(strings.Contains(errOut.String(), "Forced quit."), "Expected forced-quit message, got: %s", errOut.String())
	})
}
