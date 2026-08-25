package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	hfg "github.com/drgo/hfget"
	"github.com/drgo/hfget/testutils"
)

func TestFormatCurrentField(t *testing.T) {
	assert := testutils.NewAssert(t)
	diagnoseAfter := 3 * time.Second
	stalledAfter := 15 * time.Second

	cases := []struct {
		name         string
		idle         time.Duration
		speed        float64
		hadProgress  bool
		probed       bool
		issue        string
		want         string
		wantContains string
	}{
		{
			name:  "bytes flowing",
			idle:  time.Second,
			speed: 1024,
			want:  "Current: 1.0 KB/s",
		},
		{
			name:        "still connecting",
			idle:        4 * time.Second,
			hadProgress: false,
			probed:      false,
			want:        "connecting...",
		},
		{
			name:        "waiting for diagnosis after progress",
			idle:        4 * time.Second,
			hadProgress: true,
			probed:      false,
			want:        "waiting...",
		},
		{
			name:        "no internet",
			idle:        4 * time.Second,
			hadProgress: true,
			probed:      true,
			issue:       "no internet",
			want:        "no internet",
		},
		{
			name:        "reachable but never got bytes",
			idle:        4 * time.Second,
			hadProgress: false,
			probed:      true,
			want:        "connecting...",
		},
		{
			name:        "reachable and still no first byte after stalledAfter",
			idle:        20 * time.Second,
			hadProgress: false,
			probed:      true,
			want:        "connection stalled",
		},
		{
			name:        "had progress, host reachable, transfer stopped",
			idle:        4 * time.Second,
			hadProgress: true,
			probed:      true,
			want:        "connection stalled",
		},
		{
			name:  "issue is ignored while bytes are still flowing",
			idle:  time.Second,
			speed: 2048,
			issue: "no internet",
			want:  "Current: 2.0 KB/s",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatCurrentField(tc.idle, tc.speed, tc.hadProgress, tc.probed, tc.issue, diagnoseAfter, stalledAfter)
			if tc.want != "" {
				assert.True(got == tc.want, "%s: got %q, want %q", tc.name, got, tc.want)
			}
			if tc.wantContains != "" {
				assert.True(strings.Contains(got, tc.wantContains), "%s: got %q, want to contain %q", tc.name, got, tc.wantContains)
			}
		})
	}
}

func TestClassifyNetworkError(t *testing.T) {
	assert := testutils.NewAssert(t)

	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{context.Canceled, ""},
		{context.DeadlineExceeded, "connection timed out"},
		{&net.DNSError{Err: "no such host", Name: "huggingface.co", IsNotFound: true}, "DNS lookup failed"},
		{fmt.Errorf("dial tcp: lookup huggingface.co: no such host"), "DNS lookup failed"},
		{fmt.Errorf("dial tcp 1.2.3.4:443: connect: network is unreachable"), "no internet"},
		{fmt.Errorf("dial tcp: no route to host"), "no internet"},
		{fmt.Errorf("dial tcp 127.0.0.1:443: connect: connection refused"), "connection refused"},
		{fmt.Errorf("read: connection reset by peer"), "connection reset"},
		{fmt.Errorf("tls: handshake timeout"), "TLS handshake failed"},
		{fmt.Errorf("Get https://huggingface.co: EOF"), "network error"},
		{fmt.Errorf("download failed: %w", &net.DNSError{Err: "no such host", Name: "huggingface.co"}), "DNS lookup failed"},
	}

	for _, tc := range cases {
		got := classifyNetworkError(tc.err)
		assert.True(got == tc.want, "classifyNetworkError(%v) = %q, want %q", tc.err, got, tc.want)
	}
}

type stubConn struct{}

func (stubConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (stubConn) Write(p []byte) (int, error)      { return len(p), nil }
func (stubConn) Close() error                     { return nil }
func (stubConn) LocalAddr() net.Addr              { return stubAddr("local") }
func (stubConn) RemoteAddr() net.Addr             { return stubAddr("remote") }
func (stubConn) SetDeadline(time.Time) error      { return nil }
func (stubConn) SetReadDeadline(time.Time) error  { return nil }
func (stubConn) SetWriteDeadline(time.Time) error { return nil }

type stubAddr string

func (a stubAddr) Network() string { return "tcp" }
func (a stubAddr) String() string  { return string(a) }

func TestDiagnoseDownloadStall(t *testing.T) {
	assert := testutils.NewAssert(t)
	ctx := context.Background()
	offline := errors.New("dial tcp: network is unreachable")
	dnsFail := &net.DNSError{Err: "no such host", Name: "huggingface.co", IsNotFound: true}
	timeout := errors.New("dial tcp: i/o timeout")

	t.Run("reachable host is not an issue", func(t *testing.T) {
		dial := func(context.Context, string, string) (net.Conn, error) {
			return stubConn{}, nil
		}
		got := diagnoseDownloadStallDial(ctx, "huggingface.co", dial)
		assert.True(got == "", "got %q, want empty", got)
	})

	t.Run("unreachable network is no internet", func(t *testing.T) {
		dial := func(context.Context, string, string) (net.Conn, error) {
			return nil, offline
		}
		got := diagnoseDownloadStallDial(ctx, "huggingface.co", dial)
		assert.True(got == "no internet", "got %q", got)
	})

	t.Run("DNS fail and no IP reachability is no internet", func(t *testing.T) {
		dial := func(context.Context, string, string) (net.Conn, error) {
			return nil, dnsFail
		}
		got := diagnoseDownloadStallDial(ctx, "huggingface.co", dial)
		assert.True(got == "no internet", "got %q", got)
	})

	t.Run("DNS fail but internet works", func(t *testing.T) {
		dial := func(_ context.Context, _ string, address string) (net.Conn, error) {
			if strings.HasPrefix(address, "1.1.1.1") || strings.HasPrefix(address, "8.8.8.8") {
				return stubConn{}, nil
			}
			return nil, dnsFail
		}
		got := diagnoseDownloadStallDial(ctx, "huggingface.co", dial)
		assert.True(got == "DNS lookup failed", "got %q", got)
	})

	t.Run("host timeout but internet works", func(t *testing.T) {
		dial := func(_ context.Context, _ string, address string) (net.Conn, error) {
			if strings.HasPrefix(address, "1.1.1.1") || strings.HasPrefix(address, "8.8.8.8") {
				return stubConn{}, nil
			}
			return nil, timeout
		}
		got := diagnoseDownloadStallDial(ctx, "huggingface.co", dial)
		assert.True(got == "can't reach Hugging Face", "got %q", got)
	})

	t.Run("cancelled context", func(t *testing.T) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		got := diagnoseDownloadStallDial(canceled, "huggingface.co", func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("dial should not be called when context is already cancelled")
			return nil, nil
		})
		assert.True(got == "", "got %q", got)
	})
}

func TestDownloadDisplayProgressReplacesCurrentOnStall(t *testing.T) {
	require := testutils.NewRequire(t)
	assert := testutils.NewAssert(t)

	plan := &hfg.DownloadPlan{
		TotalDownloadSize: 1000,
		FilesToDownload: []hfg.FileDownload{
			{File: hfg.HFFile{Path: "model.bin", Size: 1000}},
		},
	}
	progress := make(chan hfg.Progress, 4)
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	diagnosed := make(chan struct{}, 1)
	cfg := downloadProgressConfig{
		tick:          20 * time.Millisecond,
		diagnoseAfter: 30 * time.Millisecond,
		stalledAfter:  time.Second,
		probeEvery:    time.Millisecond,
		host:          "huggingface.co",
		diagnose: func(context.Context, string) string {
			select {
			case diagnosed <- struct{}{}:
			default:
			}
			return "no internet"
		},
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

	select {
	case <-diagnosed:
	case <-time.After(2 * time.Second):
		t.Fatal("stall diagnosis did not run")
	}
	time.Sleep(80 * time.Millisecond)

	close(progress)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("progress display did not exit")
	}

	out := buf.String()
	require.True(strings.Contains(out, "no internet"), "expected stall reason in output, got %q", out)

	lastStatus := lastOverallStatus(out)
	assert.True(strings.Contains(lastStatus, "no internet"), "last status line %q should explain the stall", lastStatus)
	assert.False(strings.Contains(lastStatus, "Current:"), "last status line %q should not keep showing Current", lastStatus)
}

func TestDownloadDisplayProgressShowsCurrentWhileMoving(t *testing.T) {
	assert := testutils.NewAssert(t)

	plan := &hfg.DownloadPlan{
		TotalDownloadSize: 10000,
		FilesToDownload: []hfg.FileDownload{
			{File: hfg.HFFile{Path: "model.bin", Size: 10000}},
		},
	}
	progress := make(chan hfg.Progress, 8)
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := downloadProgressConfig{
		tick:          20 * time.Millisecond,
		diagnoseAfter: 200 * time.Millisecond,
		stalledAfter:  time.Second,
		probeEvery:    time.Second,
		host:          "huggingface.co",
		diagnose: func(context.Context, string) string {
			t.Error("diagnose should not run while bytes are arriving")
			return "no internet"
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		downloadDisplayProgressWith(ctx, &buf, progress, 0, plan, cfg)
	}()

	for i := int64(1); i <= 4; i++ {
		progress <- hfg.Progress{
			Filepath:    "model.bin",
			State:       hfg.ProgressStateDownloading,
			CurrentSize: i * 1000,
			TotalSize:   10000,
		}
		time.Sleep(30 * time.Millisecond)
	}
	close(progress)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("progress display did not exit")
	}

	lastStatus := lastOverallStatus(buf.String())
	assert.True(strings.Contains(lastStatus, "Current:"), "expected current speed while downloading, got %q", lastStatus)
	assert.False(strings.Contains(lastStatus, "no internet"), "did not expect stall message, got %q", lastStatus)
}

func lastOverallStatus(out string) string {
	cleaned := strings.NewReplacer(
		"\r", "\n",
		"\033[2K", "",
		"\033[A", "",
	).Replace(out)
	var last string
	for _, line := range strings.Split(cleaned, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Overall:") {
			last = line
		}
	}
	return last
}
