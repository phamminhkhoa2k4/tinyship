package caddy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tinyship/tinyship/internal/store"
)

// etagAt renders a pseudo-time ETag so the fake's tags move forward the
// way real ones do.
func etagAt(n int) string {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).
		Add(time.Duration(n) * time.Second).Format(time.RFC3339)
}

// startFake serves handler on a fresh Unix socket and returns a client
// for it plus a cleanup func.
func startFake(t *testing.T, handler http.Handler) (*Client, func()) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	return NewClient(sock), func() {
		_ = srv.Close()
		_ = ln.Close()
	}
}

func sampleServer(t *testing.T) map[string]any {
	t.Helper()
	srv, err := BuildServer([]store.RouteRow{
		{DomainID: "dom1", AppID: "appA", Hostname: "a.example.com", HostPort: 20017},
	}, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	return srv
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// fakeAdmin emulates the admin API for one server subtree: current
// config, ETags, If-Match 412 semantics.
type fakeAdmin struct {
	mu                sync.Mutex
	config            []byte // nil = subtree absent
	etag              int
	puts              int
	failPost          bool // reject every POST with 500
	conflictEveryPOST bool // reject every POST with 412
	conflictOnce      bool // reject the first POST with 412
}

func (f *fakeAdmin) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(ServerPath, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if f.config == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Etag", etagAt(f.etag))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(f.config)
		case http.MethodPost:
			f.puts++
			if f.failPost {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			// conflictOnce/conflictEveryPOST model another writer changing
			// the config underneath us.
			conflict := f.conflictEveryPOST
			if f.conflictOnce && f.puts == 1 {
				conflict = true
			}
			if conflict || (r.Header.Get("If-Match") != "" &&
				r.Header.Get("If-Match") != etagAt(f.etag)) {
				http.Error(w, "precondition failed", http.StatusPreconditionFailed)
				return
			}
			body := make([]byte, 1<<20)
			n, err := r.Body.Read(body)
			if err != nil && n == 0 {
				http.Error(w, "empty body", http.StatusBadRequest)
				return
			}
			var probe map[string]any
			if err := json.Unmarshal(body[:n], &probe); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			f.config = append([]byte(nil), body[:n]...)
			f.etag++
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func (f *fakeAdmin) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts
}

func TestApplyServerCreatesAndVerifies(t *testing.T) {
	f := &fakeAdmin{}
	c, done := startFake(t, f.handler())
	defer done()

	want := sampleServer(t)
	hash, err := c.ApplyServer(context.Background(), want)
	if err != nil {
		t.Fatalf("ApplyServer: %v", err)
	}
	wantHash, _ := Hash(want)
	if hash != wantHash {
		t.Errorf("hash = %s, want %s", hash, wantHash)
	}
	if f.putCount() != 1 {
		t.Errorf("puts = %d, want 1", f.putCount())
	}
	got, _, err := c.CurrentConfig(context.Background())
	if err != nil || got == nil {
		t.Fatalf("CurrentConfig = %v, %v", got, err)
	}
}

func TestApplyServerRetriesOn412(t *testing.T) {
	// Another writer changes the config just before our first POST, so
	// it 412s; the retry re-reads the ETag and lands.
	f := &fakeAdmin{config: []byte(`{"stale":true}`), conflictOnce: true}
	c, done := startFake(t, f.handler())
	defer done()

	if _, err := c.ApplyServer(context.Background(), sampleServer(t)); err != nil {
		t.Fatalf("ApplyServer: %v", err)
	}
	if f.putCount() < 2 {
		t.Errorf("puts = %d, want >= 2 (412 then retry)", f.putCount())
	}
}

func TestApplyServerGivesUpAfterConflicts(t *testing.T) {
	// Another writer wins every race, so every POST 412s: the loop must
	// exhaust and report E_CADDY_APPLY.
	f := &fakeAdmin{conflictEveryPOST: true}
	c, done := startFake(t, f.handler())
	defer done()

	_, err := c.ApplyServer(context.Background(), sampleServer(t))
	if err == nil || !contains(err.Error(), "E_CADDY_APPLY") {
		t.Errorf("ApplyServer = %v, want E_CADDY_APPLY after retries", err)
	}
	if f.putCount() < maxApplyAttempts {
		t.Errorf("puts = %d, want >= %d attempts", f.putCount(), maxApplyAttempts)
	}
}

// mutatingAdmin accepts the POST then serves different bytes, so the
// verification GET cannot match the desired config.
type mutatingAdmin struct {
	mu   sync.Mutex
	etag int
}

func (m *mutatingAdmin) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(ServerPath, func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Etag", etagAt(m.etag))
			_, _ = w.Write([]byte(`{"tampered":true}`))
		case http.MethodPost:
			m.etag++ // accept, then serve something else on verify
			w.WriteHeader(http.StatusOK)
		}
	})
	return mux
}

func TestApplyServerDetectsMismatch(t *testing.T) {
	m := &mutatingAdmin{}
	c, done := startFake(t, m.handler())
	defer done()

	_, err := c.ApplyServer(context.Background(), sampleServer(t))
	if err == nil || !contains(err.Error(), "E_CADDY_APPLY") {
		t.Errorf("ApplyServer = %v, want E_CADDY_APPLY on hash mismatch", err)
	}
}

func TestApplyServerUnreachableSocket(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "absent.sock"))
	_, err := c.ApplyServer(context.Background(), sampleServer(t))
	var ue *UnreachableError
	if !errors.As(err, &ue) {
		t.Errorf("ApplyServer = %v, want UnreachableError", err)
	}
}

func TestCurrentConfigMissingSubtree(t *testing.T) {
	f := &fakeAdmin{}
	c, done := startFake(t, f.handler())
	defer done()

	cfg, etag, err := c.CurrentConfig(context.Background())
	if err != nil || cfg != nil || etag != "" {
		t.Errorf("CurrentConfig = %v, %q, %v; want nil empty nil", cfg, etag, err)
	}
}

func TestApplyServerRejectsBadStatus(t *testing.T) {
	f := &fakeAdmin{failPost: true}
	c, done := startFake(t, f.handler())
	defer done()

	_, err := c.ApplyServer(context.Background(), sampleServer(t))
	if err == nil || !contains(err.Error(), "unexpected status 500") {
		t.Errorf("ApplyServer = %v, want status 500 error", err)
	}
}
