// Package health runs host-side health probes. HTTP and TCP
// probes dial the container's loopback-only published port from the host; the
// exec probe runs a command inside the container through the Docker wrapper.
// The package never talks to Docker itself except through the injected Exec
// function, so its logic is unit-testable without a daemon.
package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
)

// Failure codes surfaced in job errors. These are
// permanent: a health failure does not retry at the job level (the probe
// itself already retried per retries/interval).
const (
	EConnRefused  = "E_HEALTH_CONN_REFUSED"
	EHTTPStatus   = "E_HEALTH_HTTP_STATUS"
	EExecNonZero  = "E_HEALTH_EXEC_NONZERO"
	ETimeout      = "E_HEALTH_TIMEOUT"
	EDNSOrNetwork = "E_HEALTH_NETWORK"
	EUnstable     = "E_HEALTH_UNSTABLE" // passed, but never consecutively
)

// Result is one probe outcome.
type Result struct {
	Healthy bool
	Reason  string // empty when healthy; a failure code + detail otherwise
}

// ExecFunc runs a command inside the container and returns its exit code.
// The Docker wrapper supplies this; tests inject a fake.
type ExecFunc func(ctx context.Context, command []string, timeout time.Duration) (int, error)

// Probe runs exactly one attempt of h against target (the loopback address
// of the published port, e.g. "127.0.0.1:20042"). exec is only used for
// type=exec and may be nil there (reported as a failure).
func Probe(ctx context.Context, target string, h *appconfig.Health, exec ExecFunc) Result {
	switch h.Type {
	case "tcp":
		return probeTCP(ctx, target, h)
	case "http":
		return probeHTTP(ctx, target, h)
	case "exec":
		return probeExec(ctx, h, exec)
	default:
		// appconfig.Validate rejects unknown types; defensive here.
		return Result{false, fmt.Sprintf("%s: unknown health type %q", ETimeout, h.Type)}
	}
}

// Wait runs probes until one passes, retries are exhausted, or ctx ends
// (interval_s between attempts, retries attempts after
// start_period_s, timeout_s per attempt). It returns the last result.
func Wait(ctx context.Context, target string, h *appconfig.Health, exec ExecFunc) Result {
	return WaitStable(ctx, target, h, exec, 1)
}

// WaitStable is Wait but requires want consecutive successful probes
//
// Failure semantics: `retries` is the number of consecutive
// failures accepted ("số lần thất bại liên tiếp được chấp nhận", Docker
// healthcheck convention) — one failure more and the probe is unhealthy; a
// pass resets that counter. Because a flapping app can satisfy neither
// threshold forever, the whole probe also runs under one overall window of
// start_period_s + (retries + want − 1) × interval_s ("trong
// thời gian cho phép"); when the window closes, the result fails with the
// last reason or E_HEALTH_UNSTABLE if the last probe happened to pass.
// interval_s is ≥2 in validated configs (appconfig), so the window always
// advances.
func WaitStable(ctx context.Context, target string, h *appconfig.Health, exec ExecFunc, want int) Result {
	if want < 1 {
		want = 1
	}
	startPeriod := time.Duration(0)
	if h.StartPeriodS != nil {
		startPeriod = time.Duration(*h.StartPeriodS) * time.Second
	}
	start := time.Now()
	if startPeriod > 0 {
		select {
		case <-ctx.Done():
			return Result{false, ETimeout + ": cancelled during start period"}
		case <-time.After(startPeriod):
		}
	}
	interval := time.Duration(h.IntervalS) * time.Second
	// Overall window: long enough for the failure threshold AND a full
	// consecutive-success run (retries + want − 1 intervals), so success
	// is never cut short by the bound that exists to stop flapping.
	deadline := start.Add(startPeriod + time.Duration(h.Retries+want-1)*interval)

	passStreak, failStreak := 0, 0
	var last Result
	for attempt := 1; ; attempt++ {
		if attempt > 1 && !time.Now().Before(deadline) {
			// Overall window closed: bounded failure (never a hang).
			if last.Healthy {
				return Result{false, fmt.Sprintf("%s: passed at times but never %d "+
					"consecutively within start_period+%d×interval",
					EUnstable, want, h.Retries)}
			}
			return Result{false, fmt.Sprintf("%s: no healthy probe within the "+
				"overall window; last: %s", ETimeout, last.Reason)}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, time.Duration(h.TimeoutS)*time.Second)
		last = Probe(attemptCtx, target, h, exec)
		cancel()
		if last.Healthy {
			passStreak++
			failStreak = 0
			if passStreak >= want {
				return Result{Healthy: true}
			}
		} else {
			passStreak = 0
			failStreak++
			if failStreak >= h.Retries {
				return last
			}
		}
		if ctx.Err() != nil {
			return Result{false, ETimeout + ": cancelled"}
		}
		select {
		case <-ctx.Done():
			return Result{false, ETimeout + ": cancelled"}
		case <-time.After(interval):
		}
	}
}

func probeTCP(ctx context.Context, target string, h *appconfig.Health) Result {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return Result{false, classifyDial(err)}
	}
	conn.Close()
	return Result{Healthy: true}
}

func probeHTTP(ctx context.Context, target string, h *appconfig.Health) Result {
	url := "http://" + target + h.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{false, fmt.Sprintf("%s: %v", EDNSOrNetwork, err)}
	}
	// The client reuses the context's timeout as the per-attempt deadline.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Result{false, classifyDial(err)}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))

	lo, hi := h.ExpectStatus[0], h.ExpectStatus[1]
	if resp.StatusCode < lo || resp.StatusCode > hi {
		return Result{false, fmt.Sprintf("%s: got %d, expected %d-%d at %s",
			EHTTPStatus, resp.StatusCode, lo, hi, h.Path)}
	}
	return Result{Healthy: true}
}

func probeExec(ctx context.Context, h *appconfig.Health, exec ExecFunc) Result {
	if exec == nil {
		return Result{false, EExecNonZero + ": no exec transport available"}
	}
	code, err := exec(ctx, h.Command, time.Duration(h.TimeoutS)*time.Second)
	if err != nil {
		return Result{false, fmt.Sprintf("%s: %v", EExecNonZero, err)}
	}
	if code != 0 {
		return Result{false, fmt.Sprintf("%s: exit code %d", EExecNonZero, code)}
	}
	return Result{Healthy: true}
}

func classifyDial(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return ETimeout + ": probe timed out"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ETimeout + ": probe timed out"
	}
	if strings.Contains(err.Error(), "connection refused") {
		return EConnRefused + ": container not listening yet"
	}
	return EDNSOrNetwork + ": " + err.Error()
}
