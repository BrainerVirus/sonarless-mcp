package mcp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/idle"
	"github.com/BrainerVirus/sonarless-mcp/internal/project"
)

// WorkspaceHeader carries the client's workspace directory, so the gateway
// can default projectKey per workspace as the stdio shim does.
const WorkspaceHeader = "X-Sonarless-Workspace"

// sessionTTL drops a workspace's session after this long without requests.
const sessionTTL = time.Hour

// Gateway is the one long-lived process AI clients reach directly over
// Streamable HTTP, instead of each launching a stdio shim. Each workspace
// gets its own Shim (project defaults, sonarless_* tools, routing to remote
// servers), so behaviour matches the shim; containers start on the first
// request and every request counts as activity for the idle watcher.
type Gateway struct {
	Cfg     *config.Config
	Token   string // clients must send "Authorization: Bearer <Token>"
	Version string
	Log     io.Writer
	// NewSession builds the shim for a workspace ("" = no workspace known).
	NewSession func(workspace string) (*Shim, error)

	ctx      context.Context
	started  time.Time
	mu       sync.Mutex
	sessions map[string]*gatewaySession
	inflight atomic.Int64
}

type gatewaySession struct {
	shim *Shim
	used atomic.Int64 // unix nanos

	mu      sync.Mutex
	streams map[chan message]struct{}
	pending map[string]message // by method: notifications sent while no stream was open
}

// notify pushes a server-initiated message to the session's open event
// streams, or keeps it for the next one (a client may open its stream
// after a cold start finished).
func (s *gatewaySession) notify(m message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.streams) == 0 {
		if s.pending == nil {
			s.pending = map[string]message{}
		}
		s.pending[m.Method] = m
		return
	}
	for ch := range s.streams {
		select {
		case ch <- m:
		default: // a stuck stream must not block the shim
		}
	}
}

func (s *gatewaySession) subscribe() (chan message, func()) {
	ch := make(chan message, 8)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams == nil {
		s.streams = map[chan message]struct{}{}
	}
	s.streams[ch] = struct{}{}
	for k, m := range s.pending {
		ch <- m
		delete(s.pending, k)
	}
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.streams, ch)
	}
}

// stream serves the optional GET event stream of Streamable HTTP: server
// notifications (tools/list_changed after a cold start) for the workspace
// the client names. Clients that never open it still work: they just
// reconnect to see tools that came up late.
func (g *Gateway) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusMethodNotAllowed)
		return
	}
	s, err := g.session(r.Header.Get(WorkspaceHeader))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ch, done := s.subscribe()
	defer done()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	ping := time.NewTicker(streamPing)
	defer ping.Stop()
	for {
		select {
		case m := <-ch:
			m.JSONRPC = "2.0"
			b, _ := json.Marshal(m)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		case <-ping.C:
			s.used.Store(time.Now().UnixNano())    // an open stream keeps its session
			_, _ = io.WriteString(w, ": ping\n\n") // keeps proxies and idle timeouts away
		case <-r.Context().Done():
			return
		case <-g.ctx.Done(): // shutting down (e.g. restart after an update): let Shutdown finish
			return
		}
		flusher.Flush()
	}
}

// streamPing is how often an idle event stream gets a comment line.
var streamPing = 25 * time.Second

// ErrGatewayRunning means another gateway already serves the port.
var ErrGatewayRunning = errors.New("a sonarless-mcp gateway is already running")

// Serve listens on 127.0.0.1 and serves until ctx ends, then drains
// in-flight requests (tool calls can be slow) for up to two minutes.
func (g *Gateway) Serve(ctx context.Context) error {
	addr := fmt.Sprintf("127.0.0.1:%d", g.Cfg.Int(config.GatewayPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if GatewayAlive(ctx, g.Cfg) {
			return ErrGatewayRunning
		}
		return fmt.Errorf("gateway: %w (set %s to a free port)", err, config.GatewayPort)
	}
	if err := writeGatewayPID(g.Cfg); err != nil {
		g.logf("gateway pid file not written (setup --remove can't stop it): %v", err)
	}
	defer removeGatewayPID(g.Cfg)
	srv := &http.Server{Handler: g.Handler(ctx), ReadHeaderTimeout: 10 * time.Second}
	go g.sweep(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	g.logf("gateway listening on %s (pid %d)", g.Cfg.GatewayURL(), os.Getpid())
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return srv.Shutdown(sctx)
}

// Busy reports whether a request is being served right now.
func (g *Gateway) Busy() bool { return g.inflight.Load() > 0 }

// Handler serves /health (no token: a readiness probe), /status (sessions,
// for `sonarless-mcp status`) and the MCP endpoint. ctx bounds the
// sessions' background work (container boot).
func (g *Gateway) Handler(ctx context.Context) http.Handler {
	g.ctx, g.started = ctx, time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if !localHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": g.Version})
	})
	mux.HandleFunc(statusPath, g.serveStatus)
	mux.HandleFunc(config.GatewayPath, g.serveMCP)
	mux.HandleFunc("/mcp", g.serveMCP)
	return mux
}

func (g *Gateway) serveMCP(w http.ResponseWriter, r *http.Request) {
	// DNS rebinding and browser pages: only local hosts and local (or no) origins.
	if !localHost(r.Host) || !localOrigin(r.Header.Get("Origin")) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	if !g.authorized(r) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"sonarless-mcp gateway: missing or wrong bearer token; re-run `+"`sonarless-mcp setup`"+`"}`)
		return
	}
	if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		g.stream(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	g.inflight.Add(1)
	defer g.inflight.Add(-1)

	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	trimmed := bytes.TrimSpace(body)
	batch := len(trimmed) > 0 && trimmed[0] == '['
	msgs := decodeMessages(trimmed)
	if len(msgs) == 0 {
		writeJSON(w, message{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		return
	}
	s, err := g.session(r.Header.Get(WorkspaceHeader))
	if err != nil {
		writeJSON(w, message{JSONRPC: "2.0", ID: msgs[0].ID, Error: &rpcError{-32603, "sonarless-mcp: " + err.Error()}})
		return
	}

	var mu sync.Mutex
	var replies []message
	collect := func(m message) {
		m.JSONRPC = "2.0"
		mu.Lock()
		replies = append(replies, m)
		mu.Unlock()
	}
	var wg sync.WaitGroup
	requests := 0
	for _, m := range msgs {
		if m.Method == "" {
			continue // a response from the client: this server never asks
		}
		if m.isRequest() {
			requests++
		}
		wg.Add(1)
		go func(m message) { defer wg.Done(); s.handle(r.Context(), m, collect) }(m)
	}
	wg.Wait()
	switch {
	case requests == 0:
		w.WriteHeader(http.StatusAccepted)
	case batch:
		writeJSON(w, replies)
	case len(replies) == 1:
		writeJSON(w, replies[0])
	default: // progress or log notifications came with the answer: stream them in order
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		for _, m := range replies {
			b, _ := json.Marshal(m)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (g *Gateway) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && g.Token != "" && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(g.Token)) == 1
}

// localHost accepts the Host header of a request made to 127.0.0.1/localhost.
func localHost(hostport string) bool {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func localOrigin(origin string) bool {
	if origin == "" { // not a browser; "null" (sandboxed page, file://) is refused below
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && localHost(u.Host)
}

// workspaceKey turns the header into a project root, or "" when the client
// sent nothing usable (unexpanded placeholder, relative or missing dir).
func workspaceKey(ws string) string {
	ws = strings.TrimSpace(ws)
	if ws == "" || !filepath.IsAbs(ws) {
		return ""
	}
	if fi, err := os.Stat(ws); err != nil || !fi.IsDir() {
		return ""
	}
	return project.FindRoot(filepath.Clean(ws))
}

func (g *Gateway) session(ws string) (*gatewaySession, error) {
	key := workspaceKey(ws)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sessions == nil {
		g.sessions = map[string]*gatewaySession{}
	}
	s, ok := g.sessions[key]
	if !ok {
		shim, err := g.NewSession(key)
		if err != nil {
			return nil, err
		}
		shim.Stateless = true
		s = &gatewaySession{shim: shim}
		shim.notify = s.notify
		ctx := g.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		shim.start(ctx)
		g.sessions[key] = s
		g.logf("session for workspace %q", key)
	}
	s.used.Store(time.Now().UnixNano())
	return s, nil
}

func (s *gatewaySession) handle(ctx context.Context, m message, out func(message)) {
	s.used.Store(time.Now().UnixNano())
	s.shim.handle(ctx, m, out)
}

// sweep forgets sessions of workspaces nobody has used for a while.
func (g *Gateway) sweep(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		g.mu.Lock()
		for k, s := range g.sessions {
			if time.Since(time.Unix(0, s.used.Load())) > sessionTTL {
				delete(g.sessions, k)
			}
		}
		g.mu.Unlock()
	}
}

func (g *Gateway) logf(format string, args ...any) {
	if g.Log != nil {
		fmt.Fprintf(g.Log, "%s "+format+"\n", append([]any{time.Now().Format(time.RFC3339)}, args...)...)
	}
}

// GatewayAlive reports whether a gateway answers /health on the port.
func GatewayAlive(ctx context.Context, cfg *config.Config) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfg.GatewayHealthURL(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// EnsureGateway starts the gateway in the background unless one answers,
// and waits (up to wait) until it does. Cheap when it is already up.
func EnsureGateway(ctx context.Context, cfg *config.Config, wait time.Duration) error {
	if GatewayAlive(ctx, cfg) {
		return nil
	}
	if err := idle.Spawn(cfg, "gateway.log", "serve"); err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	for !GatewayAlive(ctx, cfg) {
		if time.Now().After(deadline) {
			return fmt.Errorf("gateway not answering on %s after %s; see %s", cfg.GatewayURL(), wait,
				filepath.Join(cfg.CacheDir, "gateway.log"))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}
