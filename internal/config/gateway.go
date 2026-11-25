package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GatewayTokenEnv is the environment variable clients that can only read
// the gateway token from the environment (Cursor, VS Code, Codex) take it from.
const GatewayTokenEnv = "SONARLESS_MCP_TOKEN"

// GatewayPath is the gateway's MCP endpoint path. It names the tool so a
// client entry pointing at it is recognisably sonarless-mcp's own.
const GatewayPath = "/sonarless-mcp"

// GatewayURL is the gateway's MCP endpoint on the host.
func (c *Config) GatewayURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", c.Int(GatewayPort), GatewayPath)
}

// GatewayHealthURL answers without a token once the gateway is up.
func (c *Config) GatewayHealthURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/health", c.Int(GatewayPort))
}

// GatewayTokenFile holds the local secret clients send to the gateway.
func (c *Config) GatewayTokenFile() string { return filepath.Join(c.ConfigDir, "mcp-gateway-token") }

// GatewayToken returns the gateway token, creating it (random, owner-only,
// no trailing newline so file references read it verbatim) on first use.
func (c *Config) GatewayToken() (string, error) {
	file := c.GatewayTokenFile()
	for i := 0; ; i++ {
		b, err := os.ReadFile(file)
		if err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return strings.TrimSpace(string(b)), nil
		}
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		if os.IsNotExist(err) {
			if err := os.MkdirAll(c.ConfigDir, 0o700); err != nil {
				return "", err
			}
			f, cerr := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if cerr == nil {
				raw := make([]byte, 32)
				if _, err := rand.Read(raw); err != nil {
					f.Close()
					return "", err
				}
				tok := hex.EncodeToString(raw)
				_, werr := f.WriteString(tok)
				if err := errors.Join(werr, f.Close()); err != nil {
					return "", err
				}
				return tok, nil
			}
			if !os.IsExist(cerr) {
				return "", cerr
			}
		}
		// Another process is creating it right now.
		if i >= 20 {
			return "", fmt.Errorf("%s is empty; delete it and retry", file)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
