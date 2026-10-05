package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/idle"
	"github.com/BrainerVirus/sonarless-mcp/internal/project"
	"github.com/BrainerVirus/sonarless-mcp/internal/sonar"
)

// cacheWait is how long initialize/tools/list wait for a cold start before
// answering from cache; MCP clients often give up on servers after ~30s.
const cacheWait = 3 * time.Second

// Backend is one SonarQube server reachable through its own shared MCP
// container: the local server, or a configured remote one.
type Backend struct {
	Name   string // "local" or the remote's name; the agent passes it as `server`
	About  string // one line for the agent, e.g. "remote CI server (branch develop)"
	APIURL string // SonarQube base URL from the host, for project lookups
	MCPURL string
	Branch string // default for tools that take a branch ("" = server's main branch)
	// Ensure starts whatever the backend needs (containers); cheap when up.
	Ensure func(ctx context.Context) error
	// Token returns a valid token; force means the last one was rejected.
	Token func(ctx context.Context, force bool) (string, error)
	// Remote backends are optional: probed before use, never on the startup path.
	Remote bool

	probeMu  sync.Mutex
	probedAt time.Time
	probeErr error

	mu      sync.Mutex // serializes boot
	ready   atomic.Bool
	token   atomic.Value // string
	inject  atomic.Bool  // the workspace's project exists here: default projectKey to it
	project string
}

// probeTTL caches reachability so an offline remote (VPN down) costs one
// quick check per window instead of a hang per call.
const probeTTL = 30 * time.Second

// reachable checks that a remote server answers, with a short timeout.
func (b *Backend) reachable(ctx context.Context) error {
	b.probeMu.Lock()
	defer b.probeMu.Unlock()
	if time.Since(b.probedAt) < probeTTL {
		return b.probeErr
	}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := sonar.NewToken(b.APIURL, "").Status(pctx)
	if err != nil {
		var apiErr *sonar.APIError
		if errors.As(err, &apiErr) {
			err = nil // it answered; auth is checked separately
		} else {
			err = fmt.Errorf("server %q (%s) is unreachable right now (VPN or network down?); the local server still works", b.Name, b.APIURL)
		}
	}
	b.probedAt, b.probeErr = time.Now(), err
	return err
}

func (b *Backend) currentToken() string {
	t, _ := b.token.Load().(string)
	return t
}

// boot makes the backend usable: containers up, token loaded, project known.
func (b *Backend) boot(ctx context.Context, p *project.Project) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.Ensure(ctx); err != nil {
		return err
	}
	if b.currentToken() == "" {
		if err := b.refresh(ctx, false); err != nil {
			return err
		}
	}
	if p != nil && b.project != p.Key {
		err := sonar.NewToken(b.APIURL, b.currentToken()).Do(ctx, http.MethodGet, "/api/components/show",
			map[string][]string{"component": {p.Key}}, nil)
		b.inject.Store(err == nil)
		b.project = p.Key
	}
	b.ready.Store(true)
	return nil
}

func (b *Backend) refresh(ctx context.Context, force bool) error {
	t, err := b.Token(ctx, force)
	if err != nil {
		return fmt.Errorf("%s SonarQube token: %w", b.Name, err)
	}
	b.token.Store(t)
	return nil
}

// Shim bridges a client's stdio to the shared HTTP MCP servers. It boots
// them on demand, keeps tokens fresh, defaults projectKey (and a remote's
// branch) for the workspace, and routes each tool call to the server the
// agent picks with the extra `server` argument.
type Shim struct {
	Cfg      *config.Config
	Backends []*Backend  // [0] is the default (local)
	Local    []LocalTool // sonarless-mcp's own tools, answered by the shim
	Project  *project.Project
	Log      io.Writer

	http      *http.Client
	out       io.Writer
	outMu     sync.Mutex
	booted    chan struct{} // closed after the default backend's first boot attempt
	bootErr   error
	touchMu   sync.Mutex
	lastTouch time.Time
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (m *message) isRequest() bool { return m.Method != "" && len(m.ID) > 0 && string(m.ID) != "null" }

func (s *Shim) local() *Backend { return s.Backends[0] }

func (s *Shim) backend(name string) *Backend {
	for _, b := range s.Backends {
		if b.Name == name {
			return b
		}
	}
	return nil
}

// Run serves MCP on in/out until in closes.
func (s *Shim) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = out
	s.http = &http.Client{} // tool calls (e.g. analysis) can be slow; rely on ctx
	s.booted = make(chan struct{})
	go func() {
		s.bootErr = s.local().boot(ctx, s.Project)
		if err := idle.EnsureWatcher(s.Cfg); err != nil && s.Log != nil {
			fmt.Fprintf(s.Log, "sonarless-mcp: idle watcher not started: %v\n", err)
		}
		close(s.booted)
	}()

	r := bufio.NewReaderSize(in, 1<<20)
	var wg sync.WaitGroup
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var msg message
			if jerr := json.Unmarshal(line, &msg); jerr != nil {
				s.send(message{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error: " + jerr.Error()}})
			} else if msg.Method != "" { // responses from the client: stateless server never asks
				wg.Add(1)
				go func() { defer wg.Done(); s.handle(ctx, msg) }()
			}
		}
		if err != nil {
			wg.Wait()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (s *Shim) handle(ctx context.Context, msg message) {
	s.touch()
	switch msg.Method {
	case "initialize", "tools/list":
		if s.waitBooted(ctx, cacheWait) != nil || s.bootErr != nil {
			if cached := s.cached(msg.Method); cached != nil {
				s.reply(msg, cached)
				return
			}
		}
	}
	if msg.Method == "tools/call" && msg.isRequest() {
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if t := s.localTool(p.Name); t != nil {
			s.runLocal(ctx, msg, t)
			return
		}
	}
	if !msg.isRequest() {
		if s.waitBooted(ctx, 0) == nil && s.bootErr == nil {
			_, _ = s.forward(ctx, s.local(), msg) // notifications: best effort
		}
		return
	}
	if err := s.waitBooted(ctx, -1); err != nil {
		return // ctx cancelled: client went away
	}
	target := s.local()
	if msg.Method == "tools/call" {
		var err error
		if target, msg.Params, err = s.route(ctx, msg.Params); err != nil {
			var te toolError
			if errors.As(err, &te) {
				s.toolFailure(msg, te.Error())
			} else {
				s.send(message{JSONRPC: "2.0", ID: msg.ID, Error: &rpcError{-32602, "sonarless-mcp: " + err.Error()}})
			}
			return
		}
	}
	replies, err := s.forward(ctx, target, msg)
	if err != nil {
		if msg.Method == "tools/call" {
			s.toolFailure(msg, err.Error())
		} else {
			s.send(message{JSONRPC: "2.0", ID: msg.ID, Error: &rpcError{-32603, "sonarless-mcp: " + err.Error()}})
		}
		return
	}
	for _, r := range replies {
		if r.Error == nil && string(r.ID) == string(msg.ID) {
			switch msg.Method {
			case "initialize":
				s.store("initialize", r.Result)
				r.Result = s.annotateInitialize(r.Result)
			case "tools/list":
				s.store("tools/list", r.Result)
				r.Result = s.annotateTools(r.Result)
			}
		}
		s.send(r)
	}
}

// route picks the backend for a tools/call (its `server` argument, default
// local), boots it if needed, and fills in that backend's defaults.
func (s *Shim) route(ctx context.Context, params json.RawMessage) (*Backend, json.RawMessage, error) {
	var p struct {
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(params, &p)
	name, _ := p.Arguments["server"].(string)
	b := s.local()
	if name != "" && name != b.Name {
		if b = s.backend(name); b == nil {
			return nil, nil, fmt.Errorf("unknown server %q (known: %s)", name, strings.Join(s.names(), ", "))
		}
	}
	if b.Remote {
		if err := b.reachable(ctx); err != nil {
			b.ready.Store(false) // re-boot (container, project check) once it is back
			return nil, nil, toolError{err}
		}
	}
	if !b.ready.Load() {
		if err := b.boot(ctx, s.Project); err != nil {
			return nil, nil, toolError{err}
		}
	}
	return b, s.fillDefaults(b, params), nil
}

// toolError is a failure the agent should see as a failed tool call (it can
// recover, e.g. by using the local server) rather than a protocol error.
type toolError struct{ error }

// toolFailure answers a tools/call with an MCP tool error result.
func (s *Shim) toolFailure(req message, text string) {
	res, _ := json.Marshal(map[string]any{
		"isError": true,
		"content": []map[string]string{{"type": "text", "text": "sonarless-mcp: " + text}},
	})
	s.send(message{JSONRPC: "2.0", ID: req.ID, Result: res})
}

func (s *Shim) names() []string {
	var out []string
	for _, b := range s.Backends {
		out = append(out, b.Name)
	}
	return out
}

// waitBooted waits for the first boot attempt: max<0 forever, 0 non-blocking.
func (s *Shim) waitBooted(ctx context.Context, max time.Duration) error {
	if max == 0 {
		select {
		case <-s.booted:
			return nil
		default:
			return errors.New("booting")
		}
	}
	var timeout <-chan time.Time
	if max > 0 {
		timeout = time.After(max)
	}
	select {
	case <-s.booted:
		return nil
	case <-timeout:
		return errors.New("still booting")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// forward POSTs one message to a backend and returns the JSON-RPC replies.
// It recovers once from a rejected token and once from stopped containers
// (e.g. after the idle watcher ran).
func (s *Shim) forward(ctx context.Context, b *Backend, msg message) ([]message, error) {
	body, _ := json.Marshal(msg)
	for attempt := 0; ; attempt++ {
		replies, status, err := s.post(ctx, b, body)
		switch {
		case err == nil && status == http.StatusUnauthorized && attempt == 0:
			if rerr := b.refresh(ctx, true); rerr != nil {
				return nil, rerr
			}
			continue
		case err != nil && attempt == 0 && ctx.Err() == nil:
			b.ready.Store(false)
			if berr := b.boot(ctx, s.Project); berr != nil {
				return nil, berr
			}
			continue
		case err != nil:
			return nil, err
		case status == http.StatusUnauthorized:
			return nil, fmt.Errorf("%s SonarQube rejected the token", b.Name)
		case status/100 != 2:
			if len(replies) > 0 {
				return replies, nil // JSON-RPC error body
			}
			return nil, fmt.Errorf("%s MCP server returned HTTP %d", b.Name, status)
		}
		return replies, nil
	}
}

func (s *Shim) post(ctx context.Context, b *Backend, body []byte) ([]message, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.MCPURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+b.currentToken())
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		return nil, resp.StatusCode, nil
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		msgs, err := readSSE(resp.Body)
		return msgs, resp.StatusCode, err
	}
	b2, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return decodeMessages(b2), resp.StatusCode, nil
}

// decodeMessages parses a JSON-RPC message or batch; garbage yields nothing.
func decodeMessages(b []byte) []message {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil
	}
	if b[0] == '[' {
		var batch []message
		_ = json.Unmarshal(b, &batch)
		return batch
	}
	var m message
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return []message{m}
}

// readSSE collects the JSON-RPC messages from a text/event-stream body.
func readSSE(r io.Reader) ([]message, error) {
	var out []message
	var data strings.Builder
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	flush := func() {
		if data.Len() > 0 {
			out = append(out, decodeMessages([]byte(data.String()))...)
			data.Reset()
		}
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	return out, sc.Err()
}

func (s *Shim) reply(req message, result json.RawMessage) {
	switch req.Method {
	case "initialize":
		result = s.annotateInitialize(result)
	case "tools/list":
		result = s.annotateTools(result)
	}
	s.send(message{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *Shim) send(m message) {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, _ = s.out.Write(append(b, '\n'))
}

// touch records activity for the idle watcher, at most every 30s.
func (s *Shim) touch() {
	s.touchMu.Lock()
	defer s.touchMu.Unlock()
	if time.Since(s.lastTouch) > 30*time.Second {
		idle.Touch(s.Cfg)
		s.lastTouch = time.Now()
	}
}

// --- response cache (answers initialize/tools/list during a cold start) ---

func (s *Shim) cacheFile() string { return filepath.Join(s.Cfg.CacheDir, "mcp-cache.json") }

func (s *Shim) cached(method string) json.RawMessage {
	b, err := os.ReadFile(s.cacheFile())
	if err != nil {
		return nil
	}
	var c map[string]json.RawMessage
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	return c[method]
}

func (s *Shim) store(method string, result json.RawMessage) {
	c := map[string]json.RawMessage{}
	if b, err := os.ReadFile(s.cacheFile()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	c[method] = result
	b, _ := json.Marshal(c)
	_ = os.MkdirAll(s.Cfg.CacheDir, 0o755)
	tmp := fmt.Sprintf("%s.%d.tmp", s.cacheFile(), os.Getpid())
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, s.cacheFile())
	}
}
