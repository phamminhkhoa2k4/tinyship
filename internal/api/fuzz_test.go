package api

// Fuzz targets for the API's input validators: arbitrary request data
// must never panic a handler or produce a server-side error; bad input
// is refused with a client error.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/tinyship/tinyship/internal/store"
)

func openFuzzStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "tinyship.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func fuzzApp(t *testing.T, s *store.Store) {
	t.Helper()
	if _, err := s.CreateApp(context.Background(), store.CreateAppInput{
		Name: "web", ConfigJSON: testConfig, Actor: "fuzz",
	}); err != nil {
		t.Fatalf("seed app: %v", err)
	}
}

func FuzzAppCreate(f *testing.F) {
	f.Add([]byte(testConfig))
	f.Add([]byte(`{"name":"web","image":"nginx","port":80}`))
	f.Add([]byte(`{"name":"bad name!","image":"nginx","port":80}`))
	f.Add([]byte(`{"name":"web","image":"nginx","port":80,"sudo":true}`))
	f.Add([]byte(`{"name":"web","image":"$(rm -rf /)","port":80}`))
	f.Add([]byte{})
	f.Add([]byte(`{`))

	f.Fuzz(func(t *testing.T, body []byte) {
		s := openFuzzStore(t)
		mux := newMux(s, t.TempDir(), slog.New(slog.DiscardHandler))
		req := httptest.NewRequest(http.MethodPost, "/apps", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code >= 500 {
			t.Fatalf("status %d for body %q: %s", rec.Code, body, rec.Body.String())
		}
	})
}

func FuzzAddDomain(f *testing.F) {
	f.Add("example.com")
	f.Add("app.example.com")
	f.Add("*.example.com")
	f.Add("example.com\nroute: catchall")
	f.Add(`example.com" } evil {`)
	f.Add("localhost")
	f.Add("")
	f.Add("a")

	f.Fuzz(func(t *testing.T, host string) {
		s := openFuzzStore(t)
		fuzzApp(t, s)
		// A fixed resolver keeps the fuzz run off the network: every
		// hostname simply does not point here.
		mux := newMuxWith(s, t.TempDir(), slog.New(slog.DiscardHandler),
			func(context.Context, string) ([]net.IP, error) { return nil, nil },
			nil, DefaultTCPAddr)
		raw, _ := json.Marshal(map[string]string{"hostname": host})
		req := httptest.NewRequest(http.MethodPost, "/apps/web/domains", bytes.NewReader(raw))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code >= 500 {
			t.Fatalf("status %d for hostname %q: %s", rec.Code, host, rec.Body.String())
		}
	})
}

func FuzzSetEnv(f *testing.F) {
	f.Add("KEY", "value")
	f.Add("KEY", "")
	f.Add("KEY", "line one\nline two")
	f.Add("bad key", "value")
	f.Add("TINYSHIP_RESERVED", "value")
	f.Add("../../etc", "value")
	f.Add("KEY", string(make([]byte, 40*1024)))

	f.Fuzz(func(t *testing.T, key, value string) {
		s := openFuzzStore(t)
		fuzzApp(t, s)
		mux := newMux(s, t.TempDir(), slog.New(slog.DiscardHandler))
		raw, _ := json.Marshal(map[string]string{"value": value})
		path := "/apps/web/env/" + url.PathEscape(key)
		req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(raw))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code >= 500 {
			t.Fatalf("status %d for env %q=%q: %s", rec.Code, key, value, rec.Body.String())
		}
	})
}
