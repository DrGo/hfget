package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

func TestConfirmRetriesInterruptedRead(t *testing.T) {
	require := testutils.NewRequire(t)
	assert := testutils.NewAssert(t)

	r := bufio.NewReader(&eintrThenReader{rest: strings.NewReader("y\n")})
	errOut := &bytes.Buffer{}
	app := &cliApp{err: errOut}
	ok, err := app.confirm(context.Background(), r, "Proceed? [y/N]: ")
	require.NoError(err, "EINTR should retry, not fail confirm")
	assert.True(ok, "expected yes after retry")
	assert.True(strings.Count(errOut.String(), "Proceed? [y/N]: ") == 2, "expected prompt reprinted after interrupt, got: %s", errOut.String())
}

type eintrThenReader struct {
	once bool
	rest io.Reader
}

func (r *eintrThenReader) Read(p []byte) (int, error) {
	if !r.once {
		r.once = true
		return 0, syscall.EINTR
	}
	return r.rest.Read(p)
}

func TestWatchSignals(t *testing.T) {
	const debounce = 30 * time.Millisecond

	t.Run("first signal does not cancel or exit", func(t *testing.T) {
		require := testutils.NewRequire(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sig := make(chan os.Signal, 2)
		stop := make(chan struct{})
		errOut := &bytes.Buffer{}
		exited := make(chan int, 1)

		go watchSignals(cancel, sig, stop, errOut, func(code int) { exited <- code }, debounce, nil)
		sig <- os.Interrupt

		select {
		case <-ctx.Done():
			t.Fatal("first signal should not cancel the context")
		case code := <-exited:
			t.Fatalf("first signal should not exit, got code %d", code)
		case <-time.After(debounce + 40*time.Millisecond):
		}
		require.True(strings.Contains(errOut.String(), "Press Ctrl+C again to exit"), "Expected first-interrupt message, got: %s", errOut.String())
		close(stop)
	})

	t.Run("first signal pauses output", func(t *testing.T) {
		require := testutils.NewRequire(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sig := make(chan os.Signal, 2)
		stop := make(chan struct{})
		errOut := &bytes.Buffer{}
		pause := &outputPause{}
		exited := make(chan int, 1)

		go watchSignals(cancel, sig, stop, errOut, func(code int) { exited <- code }, debounce, pause)
		sig <- os.Interrupt

		select {
		case <-ctx.Done():
			t.Fatal("first signal should not cancel the context")
		case code := <-exited:
			t.Fatalf("first signal should not exit, got code %d", code)
		case <-time.After(debounce + 40*time.Millisecond):
		}
		require.True(pause.Paused(), "first signal should pause progress output")
		require.True(strings.Contains(errOut.String(), "Press Ctrl+C again to exit"), "Expected first-interrupt message, got: %s", errOut.String())
		close(stop)
	})

	t.Run("queued duplicate of first signal does not quit", func(t *testing.T) {
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
		go watchSignals(cancel, sig, stop, errOut, func(code int) { exited <- code }, debounce, nil)

		select {
		case <-ctx.Done():
			t.Fatal("duplicate SIGINT from one keypress should not cancel")
		case code := <-exited:
			t.Fatalf("duplicate SIGINT from one keypress should not exit, got code %d", code)
		case <-time.After(debounce + 40*time.Millisecond):
		}
		require.True(strings.Contains(errOut.String(), "Press Ctrl+C again to exit"), "Expected first-interrupt message, got: %s", errOut.String())
		assert.False(strings.Contains(errOut.String(), "Cancelling..."), "bounce should not cancel, got: %s", errOut.String())
		assert.False(strings.Contains(errOut.String(), "Forced quit."), "bounce should not force-quit, got: %s", errOut.String())
		close(stop)
	})

	t.Run("second signal cancels without force-exit", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sig := make(chan os.Signal, 2)
		stop := make(chan struct{})
		errOut := &bytes.Buffer{}
		exited := make(chan int, 1)

		go watchSignals(cancel, sig, stop, errOut, func(code int) { exited <- code }, debounce, nil)
		sig <- os.Interrupt
		time.Sleep(debounce + 20*time.Millisecond)
		sig <- os.Interrupt

		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("second signal did not cancel the context")
		}
		require.True(strings.Contains(errOut.String(), "Cancelling..."), "Expected cancel message, got: %s", errOut.String())

		select {
		case code := <-exited:
			t.Fatalf("second signal should cancel, not force-exit, got code %d", code)
		case <-time.After(debounce + 20*time.Millisecond):
		}
		assert.False(strings.Contains(errOut.String(), "Forced quit."), "second press should not force-quit, got: %s", errOut.String())
		close(stop)
	})

	t.Run("third signal force exits", func(t *testing.T) {
		require := testutils.NewRequire(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sig := make(chan os.Signal, 2)
		stop := make(chan struct{})
		defer close(stop)
		errOut := &bytes.Buffer{}
		exited := make(chan int, 1)

		go watchSignals(cancel, sig, stop, errOut, func(code int) { exited <- code }, debounce, nil)
		sig <- os.Interrupt
		time.Sleep(debounce + 20*time.Millisecond)
		sig <- os.Interrupt
		<-ctx.Done()
		time.Sleep(debounce + 20*time.Millisecond)
		sig <- os.Interrupt

		select {
		case code := <-exited:
			require.True(code == interruptExitCode, "Expected exit code %d, got %d", interruptExitCode, code)
		case <-time.After(time.Second):
			t.Fatal("third signal did not force-exit")
		}
		require.True(strings.Contains(errOut.String(), "Forced quit."), "Expected forced-quit message, got: %s", errOut.String())
	})

	t.Run("SIGTERM cancels on first signal", func(t *testing.T) {
		require := testutils.NewRequire(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sig := make(chan os.Signal, 2)
		stop := make(chan struct{})
		errOut := &bytes.Buffer{}
		exited := make(chan int, 1)

		go watchSignals(cancel, sig, stop, errOut, func(code int) { exited <- code }, debounce, nil)
		sig <- syscall.SIGTERM

		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("SIGTERM did not cancel the context")
		}
		require.True(strings.Contains(errOut.String(), "Terminated."), "Expected terminated message, got: %s", errOut.String())

		select {
		case code := <-exited:
			t.Fatalf("first SIGTERM should not force-exit, got code %d", code)
		case <-time.After(debounce + 20*time.Millisecond):
		}
		close(stop)
	})
}

func TestProcessSIGINT(t *testing.T) {
	if os.Getenv("HFGET_SIGINT_HELPER") != "" {
		runSigintHelper()
		return
	}

	t.Run("one SIGINT does not cancel", func(t *testing.T) {
		require := testutils.NewRequire(t)
		out, code := runSigintChild(t, "one")
		require.True(code == 0, "child exit %d, output:\n%s", code, out)
		require.True(strings.Contains(out, "STILL_ALIVE"), "expected child to stay alive, got:\n%s", out)
		require.True(strings.Contains(out, "Press Ctrl+C again to exit"), "expected warning, got:\n%s", out)
	})

	t.Run("two SIGINTs cancel", func(t *testing.T) {
		require := testutils.NewRequire(t)
		out, code := runSigintChild(t, "two")
		require.True(code == 2, "child exit %d, output:\n%s", code, out)
		require.True(strings.Contains(out, "CANCELLED"), "expected cancel, got:\n%s", out)
	})
}

func runSigintHelper() {
	app := &cliApp{err: os.Stderr, exit: func(code int) {
		fmt.Fprintf(os.Stderr, "FORCED %d\n", code)
		os.Exit(code)
	}}
	ctx, stop := app.withInterrupt(context.Background())
	defer stop()
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "CANCELLED")
		os.Exit(2)
	case <-time.After(800 * time.Millisecond):
		fmt.Fprintln(os.Stderr, "STILL_ALIVE")
		os.Exit(0)
	}
}

func runSigintChild(t *testing.T, mode string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessSIGINT$", "-test.v")
	cmd.Env = append(os.Environ(), "HFGET_SIGINT_HELPER="+mode)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("first SIGINT: %v", err)
	}
	if mode == "two" {
		time.Sleep(interruptDebounce + 50*time.Millisecond)
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatalf("second SIGINT: %v", err)
		}
	}
	err := cmd.Wait()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("wait helper: %v", err)
		}
	}
	return buf.String(), code
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func waitForOutput(t *testing.T, buf *lockedBuffer, needle string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), needle) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in output %q", needle, buf.String())
}

func TestDownloadDisplayProgressPausesOnInterrupt(t *testing.T) {
	require := testutils.NewRequire(t)
	plan := &hfg.DownloadPlan{
		TotalDownloadSize: 1000,
		FilesToDownload: []hfg.FileDownload{
			{File: hfg.HFFile{Path: "model.bin", Size: 1000}},
		},
	}
	progress := make(chan hfg.Progress, 4)
	var buf lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pause := &outputPause{}
	cfg := downloadProgressConfig{
		tick:          15 * time.Millisecond,
		diagnoseAfter: time.Hour,
		stalledAfter:  time.Hour,
		probeEvery:    time.Hour,
		pause:         pause,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		downloadDisplayProgressWith(ctx, &buf, progress, 0, plan, cfg)
	}()
	progress <- hfg.Progress{
		Filepath:    "model.bin",
		State:       hfg.ProgressStateDownloading,
		CurrentSize: 100,
		TotalSize:   1000,
	}
	waitForOutput(t, &buf, "Overall:")

	pause.Print(&buf, "\nPress Ctrl+C again to exit.")
	frozen := buf.String()
	time.Sleep(80 * time.Millisecond)
	require.True(buf.String() == frozen, "progress output continued after pause:\nbefore %q\nafter %q", frozen, buf.String())
	require.True(strings.Contains(frozen, "Press Ctrl+C again to exit"), "expected interrupt message to remain, got %q", frozen)

	close(progress)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("progress display did not exit")
	}
}

func TestAnalysisDisplayProgressPausesOnInterrupt(t *testing.T) {
	require := testutils.NewRequire(t)
	progress := make(chan hfg.Progress, 4)
	var buf lockedBuffer
	pause := &outputPause{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		analysisDisplayProgress(&buf, progress, 0, 1000, "dest", pause)
	}()
	progress <- hfg.Progress{Filepath: "a.bin", CurrentSize: 100, TotalSize: 1000}
	waitForOutput(t, &buf, "Analyzing")

	pause.Print(&buf, "\nPress Ctrl+C again to exit.")
	frozen := buf.String()
	time.Sleep(250 * time.Millisecond)
	require.True(buf.String() == frozen, "analysis output continued after pause:\nbefore %q\nafter %q", frozen, buf.String())
	require.True(strings.Contains(frozen, "Press Ctrl+C again to exit"), "expected interrupt message to remain, got %q", frozen)

	close(progress)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("analysis display did not exit")
	}
}
