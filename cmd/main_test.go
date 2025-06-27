package main

import (
	"bytes"
	"context"
	"io"
	"os"
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
	m.fetchRepoInfoCalls++
	if m.repoInfoToReturn == nil {
		return &hfg.RepoInfo{ID: "test/repo"}, m.fetchErr
	}
	return m.repoInfoToReturn, m.fetchErr
}

func (m *mockDownloader) BuildPlan(ctx context.Context, repoInfo *hfg.RepoInfo) (*hfg.DownloadPlan, error) {
	m.buildPlanCalls++
	if m.planToReturn == nil {
		return &hfg.DownloadPlan{Repo: repoInfo}, m.buildErr
	}
	return m.planToReturn, m.buildErr
}

func (m *mockDownloader) ExecutePlan(ctx context.Context, plan *hfg.DownloadPlan) error {
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

	t.Run("Force flag implies quiet and skips prompt", func(t *testing.T) {
		require := testutils.NewRequire(t)
		assert := testutils.NewAssert(t)
		out := &bytes.Buffer{}
		mock := &mockDownloader{
			repoInfoToReturn: defaultRepoInfo,
			planToReturn:     defaultPlan,
		}
		app := &cliApp{
			out:           out,
			err:           &bytes.Buffer{},
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		// Use -f for force
		err := app.run([]string{"-f", "test/repo"})
		require.NoError(err, "Expected no error for forced download, got: %v", err)
		// There should be no interactive prompt in the output
		assert.False(strings.Contains(out.String(), "Proceed with download? [y/N]:"), "Expected force flag to skip the confirmation prompt")
		assert.True(mock.executePlanCalls == 1, "Expected ExecutePlan to be called once, but was called %d times", mock.executePlanCalls)
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
}
