package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
)

// statusPath answers what the gateway is doing (token required: it names
// the workspaces in use).
const statusPath = "/status"

// GatewaySession is one workspace the gateway serves.
type GatewaySession struct {
	Workspace   string    `json:"workspace"` // "" = the client sent none
	LastRequest time.Time `json:"lastRequest"`
	Streams     int       `json:"streams"` // open GET event streams
}

// GatewayState is what `status` shows about the gateway.
type GatewayState struct {
	Running  bool             `json:"running"`
	PID      int              `json:"pid,omitempty"`
	Port     int              `json:"port"`
	URL      string           `json:"url"`
	Version  string           `json:"version,omitempty"`
	Started  time.Time        `json:"started,omitzero"`
	Uptime   string           `json:"uptime,omitempty"`
	Sessions []GatewaySession `json:"sessions"`
	// Note says why sessions are unknown while it runs (no token, an older
	// gateway without /status, ...).
	Note string `json:"note,omitempty"`
}

type gatewayReport struct {
	PID      int              `json:"pid"`
	Version  string           `json:"version"`
	Started  time.Time        `json:"started"`
	Sessions []GatewaySession `json:"sessions"`
}

func (g *Gateway) serveStatus(w http.ResponseWriter, r *http.Request) {
	if !localHost(r.Host) || !localOrigin(r.Header.Get("Origin")) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	if !g.authorized(r) {
		http.Error(w, "missing or wrong bearer token", http.StatusUnauthorized)
		return
	}
	rep := gatewayReport{PID: os.Getpid(), Version: g.Version, Started: g.started, Sessions: []GatewaySession{}}
	g.mu.Lock()
	for ws, s := range g.sessions {
		s.mu.Lock()
		streams := len(s.streams)
		s.mu.Unlock()
		rep.Sessions = append(rep.Sessions, GatewaySession{Workspace: ws, LastRequest: time.Unix(0, s.used.Load()), Streams: streams})
	}
	g.mu.Unlock()
	sort.Slice(rep.Sessions, func(i, j int) bool { return rep.Sessions[i].Workspace < rep.Sessions[j].Workspace })
	writeJSON(w, rep)
}

// GatewayStatus reports the gateway on this machine: whether it runs (it
// answers on its port, or its recorded pid still runs `serve`), its pid and
// uptime, and the workspace sessions it holds. It never starts anything.
func GatewayStatus(ctx context.Context, cfg *config.Config) GatewayState {
	st := GatewayState{Port: cfg.Int(config.GatewayPort), URL: cfg.GatewayURL(), Sessions: []GatewaySession{}}
	if p, ok := readGatewayPID(cfg); ok && isGateway(processCommand(p.PID), p.Exe) {
		st.Running, st.PID, st.Started = true, p.PID, p.Started
	}
	rep, err := fetchGatewayReport(ctx, cfg)
	switch {
	case err == nil:
		st.Running, st.PID, st.Version, st.Started, st.Sessions = true, rep.PID, rep.Version, rep.Started, rep.Sessions
	case GatewayAlive(ctx, cfg):
		st.Running, st.Note = true, "sessions unknown: "+err.Error()
	case st.Running:
		st.Note = "not answering on " + st.URL
	}
	if st.Running && !st.Started.IsZero() {
		st.Uptime = time.Since(st.Started).Round(time.Second).String()
	}
	return st
}

// fetchGatewayReport asks the gateway for its sessions with the token from
// the token file; it never creates the token.
func fetchGatewayReport(ctx context.Context, cfg *config.Config) (*gatewayReport, error) {
	b, err := os.ReadFile(cfg.GatewayTokenFile())
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return nil, fmt.Errorf("no gateway token in %s", cfg.GatewayTokenFile())
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	url := fmt.Sprintf("http://127.0.0.1:%d%s", cfg.Int(config.GatewayPort), statusPath)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway %s: %s", statusPath, resp.Status)
	}
	var rep gatewayReport
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		return nil, fmt.Errorf("gateway %s: %w", statusPath, err)
	}
	if rep.Sessions == nil {
		rep.Sessions = []GatewaySession{}
	}
	return &rep, nil
}
