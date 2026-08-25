package main

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	huggingfaceHost = "huggingface.co"

	downloadStallAfter   = 3 * time.Second
	downloadStalledAfter = 15 * time.Second
	downloadProbeEvery   = 3 * time.Second
	downloadProbeTimeout = 2 * time.Second
)

type dialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

type networkDiagnoseFunc func(ctx context.Context, host string) string

// formatCurrentField is the right-hand status on the overall progress line.
// While bytes are arriving it shows current speed; once they stop it is
// replaced with a short explanation of why nothing is moving.
func formatCurrentField(idle time.Duration, currentSpeed float64, hadProgress, probed bool, issue string, diagnoseAfter, stalledAfter time.Duration) string {
	if diagnoseAfter <= 0 {
		diagnoseAfter = downloadStallAfter
	}
	if stalledAfter <= 0 {
		stalledAfter = downloadStalledAfter
	}
	if idle < diagnoseAfter {
		return "Current: " + formatSpeed(currentSpeed)
	}
	if issue != "" {
		return issue
	}
	if !probed {
		if !hadProgress {
			return "connecting..."
		}
		return "waiting..."
	}
	if !hadProgress && idle < stalledAfter {
		return "connecting..."
	}
	return "connection stalled"
}

func classifyNetworkError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return "connection timed out"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "DNS lookup failed"
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "connection timed out"
	}

	msg := strings.ToLower(err.Error())
	switch {
	case containsAny(msg, "no such host", "server misbehaving"):
		return "DNS lookup failed"
	case containsAny(msg, "network is unreachable", "network unreachable", "no route to host", "network down", "host is down"):
		return "no internet"
	case strings.Contains(msg, "connection refused"):
		return "connection refused"
	case strings.Contains(msg, "connection reset"):
		return "connection reset"
	case strings.Contains(msg, "tls:"):
		return "TLS handshake failed"
	case containsAny(msg, "i/o timeout", "timeout"):
		return "connection timed out"
	}
	return "network error"
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func defaultDial(ctx context.Context, network, address string) (net.Conn, error) {
	d := net.Dialer{Timeout: downloadProbeTimeout}
	return d.DialContext(ctx, network, address)
}

func diagnoseDownloadStall(ctx context.Context, host string) string {
	return diagnoseDownloadStallDial(ctx, host, defaultDial)
}

func diagnoseDownloadStallDial(ctx context.Context, host string, dial dialContextFunc) string {
	if ctx.Err() != nil {
		return ""
	}
	if host == "" {
		host = huggingfaceHost
	}
	if dial == nil {
		dial = defaultDial
	}

	hostCtx, hostCancel := context.WithTimeout(ctx, downloadProbeTimeout)
	conn, err := dial(hostCtx, "tcp", net.JoinHostPort(host, "443"))
	hostCancel()
	if err == nil {
		_ = conn.Close()
		return ""
	}

	reason := classifyNetworkError(err)
	if reason == "no internet" {
		return reason
	}

	netCtx, netCancel := context.WithTimeout(ctx, downloadProbeTimeout)
	reachable := internetReachable(netCtx, dial)
	netCancel()
	if !reachable {
		return "no internet"
	}

	switch reason {
	case "DNS lookup failed":
		return "DNS lookup failed"
	case "connection refused", "connection reset", "TLS handshake failed":
		return reason
	default:
		return "can't reach Hugging Face"
	}
}

func internetReachable(ctx context.Context, dial dialContextFunc) bool {
	for _, addr := range []string{"1.1.1.1:443", "8.8.8.8:443"} {
		if ctx.Err() != nil {
			return false
		}
		conn, err := dial(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return true
		}
	}
	return false
}

type stallMonitor struct {
	host     string
	diagnose networkDiagnoseFunc
	interval time.Duration

	mu      sync.Mutex
	issue   string
	probed  bool
	probing bool
	lastTry time.Time
	gen     uint64
}

func (m *stallMonitor) snapshot() (issue string, probed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.issue, m.probed
}

func (m *stallMonitor) clear() {
	m.mu.Lock()
	m.issue = ""
	m.probed = false
	m.lastTry = time.Time{}
	m.gen++
	m.mu.Unlock()
}

func (m *stallMonitor) kick(ctx context.Context) {
	if m.diagnose == nil {
		m.diagnose = diagnoseDownloadStall
	}
	interval := m.interval
	if interval <= 0 {
		interval = downloadProbeEvery
	}

	m.mu.Lock()
	if m.probing || (!m.lastTry.IsZero() && time.Since(m.lastTry) < interval) {
		m.mu.Unlock()
		return
	}
	m.probing = true
	gen := m.gen
	host := m.host
	diagnose := m.diagnose
	m.mu.Unlock()

	go func() {
		issue := diagnose(ctx, host)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.probing = false
		if gen != m.gen {
			return
		}
		m.issue = issue
		m.probed = true
		m.lastTry = time.Now()
	}()
}
