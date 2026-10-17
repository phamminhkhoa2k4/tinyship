package worker

// Log server: the read-only channel API → worker for container logs: a
// Unix socket only the worker can open. The worker is the only component
// holding Docker access, so ownership checks happen here and refusals
// are audited. The server speaks HTTP over its Unix socket so follow
// streams reuse plain request semantics; it never listens on TCP.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/tinyship/tinyship/internal/docker"
)

// LogSockName is the worker's log socket inside the run directory,
// created with mode 0660.
const LogSockName = "worker.sock"

// DefaultLogTail bounds how many lines a request without an explicit tail
// returns; --tail 0 means everything (the wrapper maps ≤0 to "all").
const DefaultLogTail = 100

// LogServer serves container logs over the worker's Unix socket.
type LogServer struct {
	Store    logStore
	Docker   Docker
	Actor    string
	SockPath string
	Logger   *slog.Logger

	server *http.Server
	ln     net.Listener
}

// logStore is the slice of store.Store the log server needs (the concrete
// *store.Store satisfies it).
type logStore interface {
	DeploymentOwnsContainer(ctx context.Context, appID, containerID string) (bool, error)
	AppendAudit(ctx context.Context, actor, action, targetType, targetID, result string, detail any) error
}

// NewLogServer creates the run directory if needed, removes a stale socket
// from a previous unclean shutdown, and starts listening with mode 0660.
// It returns once the socket accepts connections.
func NewLogServer(s logStore, d Docker, runDir, actor string, log *slog.Logger) (*LogServer, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		return nil, fmt.Errorf("logserver: mkdir %s: %w", runDir, err)
	}
	sock := filepath.Join(runDir, LogSockName)
	// A socket file left behind by a killed worker would block re-binding;
	// removal is safe because the previous owner is gone by construction
	// (systemd runs one worker; see plan notes for the single-instance rule).
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("logserver: listen %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o660); err != nil {
		ln.Close()
		return nil, fmt.Errorf("logserver: chmod %s: %w", sock, err)
	}
	ls := &LogServer{
		Store: s, Docker: d, Actor: actor, SockPath: sock, Logger: log,
		ln: ln,
	}
	ls.server = &http.Server{Handler: ls.Handler()}
	return ls, nil
}

// Serve blocks until ctx is cancelled, then closes the listener and removes
// the socket file.
func (l *LogServer) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = l.server.Shutdown(shutdownCtx)
	}()
	l.Logger.Info("worker: log socket ready", "path", l.SockPath)
	err := l.server.Serve(l.ln)
	_ = os.Remove(l.SockPath)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("logserver: serve: %w", err)
	}
	return nil
}

// Close releases the listener immediately (tests).
func (l *LogServer) Close() error {
	_ = l.server.Close()
	_ = os.Remove(l.SockPath)
	return nil
}

// Handler exposes the routes for tests.
func (l *LogServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /logs", l.handleLogs)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// handleLogs streams container logs after the ownership checks.
//
// Query: app_id, container_id (both required), tail (default 100, ≤0 = all),
// since (Go duration, e.g. 15m), follow (1/true).
func (l *LogServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	appID := q.Get("app_id")
	containerID := q.Get("container_id")
	if appID == "" || containerID == "" {
		writeLogError(w, http.StatusBadRequest, "E_VALIDATION",
			"app_id and container_id are required")
		return
	}
	tail := DefaultLogTail
	if raw := q.Get("tail"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeLogError(w, http.StatusBadRequest, "E_VALIDATION",
				fmt.Sprintf("tail %q must be an integer (≤0 for all lines)", raw))
			return
		}
		tail = n
	}
	var since time.Duration
	if raw := q.Get("since"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			writeLogError(w, http.StatusBadRequest, "E_VALIDATION",
				fmt.Sprintf("since %q must be a non-negative Go duration (e.g. 15m)", raw))
			return
		}
		since = d
	}
	follow := q.Get("follow") == "1" || q.Get("follow") == "true"

	// all three ownership checks before reading logs; refusals
	// are audited with the reason (request data is
	// untrusted input).
	owns := l.Store.DeploymentOwnsContainer
	if err := docker.AssertOwned(r.Context(), l.Docker.InspectContainer, appID, containerID, owns); err != nil {
		reason := "ownership check failed"
		if !errors.Is(err, docker.ErrNotManaged) {
			reason = err.Error() // DB trouble, still refuse the read
		}
		_ = l.Store.AppendAudit(r.Context(), l.Actor, "logs.read", "container",
			containerID, "denied", map[string]any{
				"app_id": appID, "reason": reason, "error": err.Error(),
			})
		l.Logger.Warn("log read refused", "container", containerID, "app", appID, "err", err)
		writeLogError(w, http.StatusForbidden, "E_NOT_MANAGED", err.Error())
		return
	}

	rc, err := l.Docker.ContainerLogs(r.Context(), containerID, tail, since, follow)
	if err != nil {
		writeLogError(w, http.StatusBadGateway, "E_LOGS_UNAVAILABLE", err.Error())
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := rc.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return // client hung up (Ctrl-C on follow)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

func writeLogError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}
