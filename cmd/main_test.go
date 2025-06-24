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
)

// mockDownloader is a mock implementation of the downloader interface for testing.
// It can be configured to fail a certain number of times before succeeding.
type mockDownloader struct {
	planToReturn *hfg.DownloadPlan
	errToReturn  error

	// For retry tests
	executePlanFailures int
	executePlanAttempts int

	// For interactive re-download tests
	getPlanAttempts int
	emptyPlan       *hfg.DownloadPlan
	fullPlan        *hfg.DownloadPlan
}

func (m *mockDownloader) GetDownloadPlan(ctx context.Context) (*hfg.DownloadPlan, error) {
	m.getPlanAttempts++
	// Special logic for the interactive re-download test
	if m.emptyPlan != nil && m.fullPlan != nil {
		if m.getPlanAttempts == 1 {
			return m.emptyPlan, nil
		}
		return m.fullPlan, nil
	}

	if m.errToReturn != nil && m.planToReturn == nil {
		return nil, m.errToReturn
	}
	return m.planToReturn, nil
}

func (m *mockDownloader) ExecutePlan(ctx context.Context, plan *hfg.DownloadPlan) error {
	m.executePlanAttempts++
	if m.executePlanAttempts <= m.executePlanFailures {
		return m.errToReturn
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
		FilesToDownload: []hfg.HFFile{
			{Path: "file1.txt", Size: 1024},
			{Path: "file2.bin", Size: 2048},
		},
		TotalSize: 3072,
	}

	t.Run("Missing repository argument", func(t *testing.T) {
		app := &cliApp{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
		err := app.run([]string{})
		if err == nil {
			t.Fatal("Expected an error for missing argument, but got none")
		}
		if !strings.Contains(err.Error(), "argument is required") {
			t.Errorf("Expected error message to contain 'argument is required', got: %v", err)
		}
	})

	t.Run("Show version flag", func(t *testing.T) {
		out := &bytes.Buffer{}
		app := &cliApp{out: out, err: &bytes.Buffer{}}
		err := app.run([]string{"--version"})
		if err != nil {
			t.Fatalf("Expected no error when showing version, got: %v", err)
		}
		if !strings.Contains(out.String(), "hfget version") {
			t.Errorf("Expected output to contain version info, got: %s", out.String())
		}
	})

	t.Run("Force flag skips prompt", func(t *testing.T) {
		out := &bytes.Buffer{}
		mock := &mockDownloader{planToReturn: defaultPlan}
		app := &cliApp{
			out:           out,
			err:           &bytes.Buffer{},
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		err := app.run([]string{"-f", "test/repo"})
		if err != nil {
			t.Fatalf("Expected no error for forced download, got: %v", err)
		}
		if strings.Contains(out.String(), "Proceed with download? [y/N]:") {
			t.Error("Expected force flag to skip the confirmation prompt")
		}
		if mock.executePlanAttempts != 1 {
			t.Errorf("Expected ExecutePlan to be called once, but was called %d times", mock.executePlanAttempts)
		}
	})

	t.Run("No files to download", func(t *testing.T) {
		out := &bytes.Buffer{}
		emptyPlan := &hfg.DownloadPlan{FilesToDownload: []hfg.HFFile{}}
		app := &cliApp{
			out:           out,
			err:           &bytes.Buffer{},
			newDownloader: func(string, ...hfg.Option) downloader { return &mockDownloader{planToReturn: emptyPlan} },
		}

		err := app.run([]string{"test/repo"})
		if err != nil {
			t.Fatalf("Expected no error when no files need downloading, got: %v", err)
		}
		if !strings.Contains(out.String(), "All model files are already present and valid") {
			t.Error("Expected to see the 'nothing to download' message")
		}
	})

	t.Run("Interactive prompt to re-download", func(t *testing.T) {
		restore := mockStdin(t, "y\n") // Simulate the user typing "y" and pressing Enter
		defer restore()

		out := &bytes.Buffer{}
		mock := &mockDownloader{
			emptyPlan: &hfg.DownloadPlan{Repo: &hfg.RepoInfo{ID: "test/repo"}, FilesToDownload: []hfg.HFFile{}},
			fullPlan:  defaultPlan,
		}
		app := &cliApp{
			out:           out,
			err:           &bytes.Buffer{},
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		err := app.run([]string{"test/repo"})
		if err != nil {
			t.Fatalf("Expected no error after re-download confirmation, got: %v", err)
		}

		if mock.getPlanAttempts != 2 {
			t.Errorf("Expected GetDownloadPlan to be called twice, but was called %d times", mock.getPlanAttempts)
		}
		if mock.executePlanAttempts != 1 {
			t.Errorf("Expected ExecutePlan to be called once, but was called %d times", mock.executePlanAttempts)
		}
		if !strings.Contains(out.String(), "Would you like to force a re-download anyway?") {
			t.Error("Expected the interactive re-download prompt to be shown")
		}
	})

	t.Run("Retry on transient error", func(t *testing.T) {
		out := &bytes.Buffer{}
		// This mock will fail once with a transient error, then succeed.
		mock := &mockDownloader{
			planToReturn:        defaultPlan,
			errToReturn:         os.ErrDeadlineExceeded, // A generic transient error
			executePlanFailures: 1,                      // Fail on the first attempt
		}
		app := &cliApp{
			out:           out,
			err:           &bytes.Buffer{},
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		// Use a very short retry interval for the test
		err := app.run([]string{"--retry-interval", "1ms", "-f", "test/repo"})
		if err != nil {
			t.Fatalf("Expected no final error after retry, got: %v", err)
		}

		if mock.executePlanAttempts != 2 {
			t.Errorf("Expected ExecutePlan to be called 2 times, but was called %d times", mock.executePlanAttempts)
		}
		if !strings.Contains(out.String(), "Attempt 2 of 3") {
			t.Error("Expected to see the retry attempt message in the logs")
		}
	})

	t.Run("No retry on fatal error", func(t *testing.T) {
		out := &bytes.Buffer{}
		// This mock will fail once with a fatal error.
		mock := &mockDownloader{
			planToReturn:        defaultPlan,
			errToReturn:         hfg.ErrAuthentication, // A fatal error
			executePlanFailures: 1,
		}
		app := &cliApp{
			out:           out,
			err:           &bytes.Buffer{},
			newDownloader: func(string, ...hfg.Option) downloader { return mock },
		}

		err := app.run([]string{"-f", "test/repo"})
		if err == nil {
			t.Fatal("Expected a fatal error, but got none")
		}

		if mock.executePlanAttempts != 1 {
			t.Errorf("Expected ExecutePlan to be called only once, but was called %d times", mock.executePlanAttempts)
		}
		if strings.Contains(out.String(), "Attempt 2 of 3") {
			t.Error("Expected not to see a retry attempt message in the logs")
		}
	})
}
