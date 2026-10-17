package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tinyship/tinyship/internal/appconfig"
)

// fastHealth returns a health config with production semantics but timings
// small enough for tests (interval 2s is the production minimum; tests use
// direct Probe or a Wait with interval at the boundary).
func fastHealth(typ string) *appconfig.Health {
	h := &appconfig.Health{
		Type: typ, Port: 1234,
		IntervalS: 2, TimeoutS: 1, Retries: 3,
	}
	return h
}

func TestProbeHTTPSuccessAndStatusRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(204)
		case "/teapot":
			w.WriteHeader(418)
		}
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")

	h := fastHealth("http")
	h.Path = "/ok"
	h.ExpectStatus = [2]int{200, 299}
	if res := Probe(context.Background(), host, h, nil); !res.Healthy {
		t.Errorf("204 within 200-299: got unhealthy %q", res.Reason)
	}

	h.Path = "/teapot"
	h.ExpectStatus = [2]int{200, 399}
	res := Probe(context.Background(), host, h, nil)
	if res.Healthy {
		t.Error("418 outside expected range must be unhealthy")
	}
	if !strings.Contains(res.Reason, EHTTPStatus) || !strings.Contains(res.Reason, "418") {
		t.Errorf("reason = %q, want %s with the actual code", res.Reason, EHTTPStatus)
	}
}

func TestProbeHTTPConnectionRefused(t *testing.T) {
	// Grab a port then close it so nothing listens.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	h := fastHealth("http")
	h.Path = "/"
	res := Probe(context.Background(), addr, h, nil)
	if res.Healthy {
		t.Fatal("closed port must be unhealthy")
	}
	if !strings.Contains(res.Reason, EConnRefused) {
		t.Errorf("reason = %q, want %s", res.Reason, EConnRefused)
	}
}

func TestProbeTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	h := fastHealth("tcp")
	if res := Probe(context.Background(), ln.Addr().String(), h, nil); !res.Healthy {
		t.Errorf("listening port: %q", res.Reason)
	}

	// Closed port.
	dead := net.JoinHostPort("127.0.0.1", "1")
	if res := Probe(context.Background(), dead, h, nil); res.Healthy {
		t.Error("closed port must be unhealthy")
	}
}

func TestProbeExecExitCodes(t *testing.T) {
	h := fastHealth("exec")
	h.Command = []string{"true"}

	if res := Probe(context.Background(), "", h, func(context.Context, []string, time.Duration) (int, error) {
		return 0, nil
	}); !res.Healthy {
		t.Errorf("exit 0 must be healthy: %q", res.Reason)
	}

	res := Probe(context.Background(), "", h, func(context.Context, []string, time.Duration) (int, error) {
		return 2, nil
	})
	if res.Healthy || !strings.Contains(res.Reason, EExecNonZero) || !strings.Contains(res.Reason, "2") {
		t.Errorf("exit 2: healthy=%v reason=%q", res.Healthy, res.Reason)
	}

	res = Probe(context.Background(), "", h, func(context.Context, []string, time.Duration) (int, error) {
		return 0, errors.New("exec transport broke")
	})
	if res.Healthy || !strings.Contains(res.Reason, "exec transport broke") {
		t.Errorf("transport error: %q", res.Reason)
	}

	// Nil exec transport must not panic.
	res = Probe(context.Background(), "", h, nil)
	if res.Healthy {
		t.Error("nil exec transport must be unhealthy")
	}
}

// TestWaitRetriesUntilSuccess covers retry semantics: failures are
// retried up to retries, then the last failure is returned with its code.
func TestWaitRetriesUntilSuccess(t *testing.T) {
	// A listener that refuses first, then accepts: simulate by port that
	// opens late.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // start closed

	h := fastHealth("tcp")
	h.IntervalS = 2
	h.Retries = 3
	h.TimeoutS = 1

	// Open the port shortly after Wait starts.
	time.AfterFunc(1500*time.Millisecond, func() {
		ln2, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		go func() {
			for {
				c, err := ln2.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		t.Cleanup(func() { ln2.Close() })
	})

	res := Wait(context.Background(), addr, h, nil)
	if !res.Healthy {
		t.Errorf("probe should have succeeded after retries: %q", res.Reason)
	}
}

func TestWaitExhaustsRetries(t *testing.T) {
	h := fastHealth("tcp")
	h.IntervalS = 2
	h.Retries = 2
	h.TimeoutS = 1

	start := time.Now()
	res := Wait(context.Background(), "127.0.0.1:1", h, nil)
	if res.Healthy {
		t.Fatal("closed port never becomes healthy")
	}
	if !strings.Contains(res.Reason, EConnRefused) {
		t.Errorf("reason = %q", res.Reason)
	}
	// Two attempts with one interval between them: ≥2s, and bounded (would
	// hang if retry logic were wrong).
	if elapsed := time.Since(start); elapsed < 2*time.Second || elapsed > 10*time.Second {
		t.Errorf("Wait took %v, want ~2-3s for 2 retries at interval 2s", elapsed)
	}
}

func TestWaitHonorsCancellation(t *testing.T) {
	h := fastHealth("tcp")
	h.IntervalS = 2
	h.Retries = 10
	h.TimeoutS = 1
	h.StartPeriodS = nil // no start period: probe immediately

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	start := time.Now()
	res := Wait(ctx, "127.0.0.1:1", h, nil)
	if res.Healthy {
		t.Fatal("cancelled wait must not report healthy")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("Wait ignored cancellation: took %v", time.Since(start))
	}
}

// TestWaitStableRequiresConsecutivePasses covers health: success
// needs 2 consecutive passes after start_period_s, while the probe treats
// `retries` as accepted *consecutive* failures — a flapping probe resets
// both counters and is stopped by the overall window instead.
func TestWaitStableRequiresConsecutivePasses(t *testing.T) {
	// Stable pass: two consecutive ok probes converge on attempt 2.
	h := fastHealth("exec")
	h.Command = []string{"probe"}
	h.Retries = 3
	h.IntervalS = 1 // fastest pace that still matches production shape
	h.TimeoutS = 1

	calls := 0
	stable := func(context.Context, []string, time.Duration) (int, error) {
		calls++
		return 0, nil
	}
	res := WaitStable(context.Background(), "", h, stable, 2)
	if !res.Healthy || calls != 2 {
		t.Errorf("stable: healthy=%v calls=%d, want true/2", res.Healthy, calls)
	}

	// Flapping F,P,F,P,...: never 2 consecutive passes nor 3 consecutive
	// failures; the overall window (start_period 0 + (retries+want-1)×interval)
	// bounds the job and the result fails.
	calls = 0
	flap := func(context.Context, []string, time.Duration) (int, error) {
		calls++
		if calls%2 == 0 {
			return 0, nil // even attempts pass, odd fail: streaks never reach either threshold
		}
		return 1, nil
	}
	start := time.Now()
	res = WaitStable(context.Background(), "", h, flap, 2)
	if res.Healthy {
		t.Error("flapping probe must not report healthy without 2 consecutive passes")
	}
	// Last probe (4th) passes on its own — must still be a failure, and
	// specifically E_HEALTH_UNSTABLE, never a silent healthy.
	if !strings.Contains(res.Reason, EUnstable) {
		t.Errorf("reason = %q, want %s", res.Reason, EUnstable)
	}
	if wantCalls := h.Retries + 2 - 1; calls != wantCalls {
		t.Errorf("calls = %d, want the window to stop at %d attempts", calls, wantCalls)
	}
	if elapsed := time.Since(start); elapsed < 3*time.Second || elapsed > 10*time.Second {
		t.Errorf("flapping window took %v, want ~4s ((retries+want-1)×interval)", elapsed)
	}

	// retries=1 accepts one consecutive failure: continuous failure fails
	// immediately with the real reason...
	fail := fastHealth("tcp")
	fail.Retries = 1
	fail.IntervalS = 1
	fail.TimeoutS = 1
	res = WaitStable(context.Background(), "127.0.0.1:1", fail, nil, 2)
	if res.Healthy || !strings.Contains(res.Reason, EConnRefused) {
		t.Errorf("retries=1 closed port = %+v, want immediate %s", res, EConnRefused)
	}
	// ...yet a clean two-pass run still converges: want > retries is not a
	// config error, the failure counter simply never engages.
	okExec := fastHealth("exec")
	okExec.Command = []string{"probe"}
	okExec.Retries = 1
	okExec.IntervalS = 1
	okExec.TimeoutS = 1
	res = WaitStable(context.Background(), "", okExec, func(context.Context, []string, time.Duration) (int, error) {
		return 0, nil
	}, 2)
	if !res.Healthy {
		t.Errorf("clean 2-pass run with retries=1 = %+v, want healthy", res)
	}
}
