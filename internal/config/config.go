// Package config resolves goxray's runtime configuration from flags and env.
package config

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
)

// Config is the resolved runtime configuration.
type Config struct {
	Port         int
	UIPort       int      // dashboard port; 0 disables it entirely
	Home         string   // $GOXRAY_HOME, default ~/.goxray
	Upstream     *url.URL // resolved upstream base (scheme://host:port)
	MaxBodyBytes int64
	NoBrowser    bool // reserved; MVP has no browser
}

// UIEnabled reports whether the read-only dashboard should be served.
//
// Off by default and opt-in per run: the dashboard exposes every recorded
// conversation over HTTP, so it is not something a user should get without
// asking for it.
func (c *Config) UIEnabled() bool { return c.UIPort > 0 }

// Load parses flags + env into a Config. args excludes the program name.
func Load(args []string) (*Config, error) {
	fs := flag.NewFlagSet("goxray", flag.ContinueOnError)
	port := fs.Int("port", envInt("PROXY_PORT", 5599), "proxy listen port")
	uiPort := fs.Int("ui-port", envInt("GOXRAY_UI_PORT", 0), "dashboard listen port (0 = disabled)")
	home := fs.String("home", envStr("GOXRAY_HOME", ""), "data directory (default ~/.goxray)")
	maxBodyMB := fs.Int("max-body-mb", envInt("GOXRAY_MAX_BODY_MB", 50), "max request body size in MiB")
	upstream := fs.String("upstream", envStr("GOXRAY_UPSTREAM", ""), "override upstream base URL")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if *uiPort != 0 && *uiPort == *port {
		return nil, fmt.Errorf("--ui-port %d collides with the proxy port", *uiPort)
	}

	c := &Config{
		Port:         *port,
		UIPort:       *uiPort,
		MaxBodyBytes: int64(*maxBodyMB) * 1024 * 1024,
	}

	if *home != "" {
		c.Home = *home
	} else {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home dir: %w", err)
		}
		c.Home = filepath.Join(h, ".goxray")
	}

	up, err := resolveUpstream(*upstream)
	if err != nil {
		return nil, err
	}
	c.Upstream = up
	return c, nil
}

// resolveUpstream picks the upstream base URL. Precedence:
// explicit --upstream/GOXRAY_UPSTREAM > ANTHROPIC_TEST_* (test/mock override) > default.
func resolveUpstream(explicit string) (*url.URL, error) {
	if explicit != "" {
		return url.Parse(explicit)
	}
	if h := os.Getenv("ANTHROPIC_TEST_HOST"); h != "" {
		proto := envStr("ANTHROPIC_TEST_PROTOCOL", "http")
		port := envStr("ANTHROPIC_TEST_PORT", "80")
		return url.Parse(fmt.Sprintf("%s://%s:%s", proto, h, port))
	}
	return url.Parse("https://api.anthropic.com")
}

// BaseURLLine returns the export line the user should set for Claude Code.
func (c *Config) BaseURLLine() string {
	return fmt.Sprintf("ANTHROPIC_BASE_URL=http://localhost:%d", c.Port)
}

func (c *Config) BlobsDir() string     { return filepath.Join(c.Home, "blobs") }
func (c *Config) ManifestsDir() string { return filepath.Join(c.Home, "manifests") }
func (c *Config) ArchivesDir() string  { return filepath.Join(c.Home, "archives") }
func (c *Config) IndexPath() string    { return filepath.Join(c.Home, "index.db") }

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
