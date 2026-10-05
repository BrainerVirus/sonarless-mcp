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

// Shim bridges a client's stdio to the shared HTTP MCP server. It boots the
// shared containers on demand, keeps the token fresh, and defaults
// projectKey to the workspace's project.
type Shim struct {
	Cfg     *config.Config
	Server  *sonar.Server
	MCP     *Container
	Project *project.Project
	Log     io.Writer

	http      *http.Client
	out       io.Writer
	outMu     sync.Mutex
	bootMu    sync.Mutex
	booted    chan struct{} // closed after the first boot attempt finishes
	bootOnce  sync.Once
	bootErr   error
	token     string
	tokenMu   sync.Mutex
	inject    atomic.Bool // project exists on the server: default projectKey to it
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

// Run serves MCP on in/out until in closes.
func (s *Shim) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = out
	s.http = &http.Client{} // tool calls (e.g. analysis) can be slow; rely on ctx
	s.booted = make(chan struct{})
	go func() {
		s.bootErr = s.ensure(ctx)
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
	if !msg.isRequest() {
		if s.waitBooted(ctx, 0) == nil && s.bootErr == nil {
			_, _ = s.forward(ctx, msg) // notifications: best effort
		}
		return
	}
	if err := s.waitBooted(ctx, -1); err != nil {
		return // ctx cancelled: client went away
	}
	if msg.Method == "tools/call" {
		msg.Params = s.injectProject(msg.Params)
	}
	replies, err := s.forward(ctx, msg)
	if err != nil {
		s.send(message{JSONRPC: "2.0", ID: msg.ID, Error: &rpcError{-32603, "sonarless-mcp: " + err.Error()}})
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

// ensure makes sure the server, MCP container and token are ready, and
// starts the idle watcher. Serialized; cheap when everything is already up.
func (s *Shim) ensure(ctx context.Context) error {
	s.bootMu.Lock()
	defer s.bootMu.Unlock()
	if !s.MCP.Alive(ctx) || !s.Server.Running(ctx) {
		if err := s.Server.Ensure(ctx); err != nil {
			return err
		}
		if err := s.MCP.Ensure(ctx); err != nil {
			return err
		}
	}
	if err := s.refreshToken(ctx, false); err != nil {
		return err
	}
	if err := idle.EnsureWatcher(s.Cfg); err != nil {
		fmt.Fprintf(s.Log, "sonarless-mcp: idle watcher not started: %v\n", err)
	}
	if s.Project != nil {
		err := sonar.NewToken(s.Cfg.ServerURL(), s.currentToken()).Do(ctx, http.MethodGet, "/api/components/show",
			map[string][]string{"component": {s.Project.Key}}, nil)
		s.inject.Store(err == nil)
	}
	return nil
}

func (s *Shim) currentToken() string {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	return s.token
}

func (s *Shim) refreshToken(ctx context.Context, force bool) error {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	if s.token != "" && !force {
		return nil
	}
	t, err := s.Server.Token(ctx)
	if err != nil {
		return fmt.Errorf("get SonarQube token: %w", err)
	}
	s.token = t
	return nil
}

// forward POSTs one message upstream and returns the JSON-RPC replies. It
// recovers once from an expired token and once from stopped containers
// (e.g. after the idle watcher ran).
func (s *Shim) forward(ctx context.Context, msg message) ([]message, error) {
	body, _ := json.Marshal(msg)
	for attempt := 0; ; attempt++ {
		replies, status, err := s.post(ctx, body)
		switch {
		case err == nil && status == http.StatusUnauthorized && attempt == 0:
			if rerr := s.refreshToken(ctx, true); rerr != nil {
				return nil, rerr
			}
			continue
		case err != nil && attempt == 0 && ctx.Err() == nil:
			if eerr := s.ensure(ctx); eerr != nil {
				return nil, eerr
			}
			continue
		case err != nil:
			return nil, err
		case status/100 != 2:
			if len(replies) > 0 {
				return replies, nil // JSON-RPC error body
			}
			return nil, fmt.Errorf("MCP server returned HTTP %d", status)
		}
		return replies, nil
	}
}

func (s *Shim) post(ctx context.Context, body []byte) ([]message, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Cfg.MCPURL(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+s.currentToken())
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
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return decodeMessages(b), resp.StatusCode, nil
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
