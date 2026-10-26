// Package proxy is the transparent HTTP front end: it accepts Claude Code's
// requests, drives the fail-closed recording sequence, and forwards to the
// Anthropic upstream. All CPU-light; the durability barriers live in the
// injected Recorder.
package proxy

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.inndy.tw/goxray/internal/config"
	"go.inndy.tw/goxray/internal/model"
)

// Recorder is the subset of store.Recorder the transport needs. Declared here
// (not imported) so transports and tests can inject a fake.
type Recorder interface {
	NewExchange(method, rawURL string, h http.Header) *model.Exchange
	PersistRequest(ex *model.Exchange, rawReq []byte) error
	PersistResponse(ex *model.Exchange, rawRes []byte) error
}

// Server is the proxy HTTP server.
type Server struct {
	cfg    *config.Config
	rec    Recorder
	client *http.Client
	tr     *httpTransport
}

// New builds a Server. The http.Client is tuned for streaming: no overall
// Timeout (that would kill long turns), compression disabled (so captured bytes
// are the identity-encoded wire bytes), header timeout only.
func New(cfg *config.Config, rec Recorder) *Server {
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DisableCompression:    true,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
		},
	}
	return &Server{cfg: cfg, rec: rec, client: client, tr: &httpTransport{}}
}

// Handler returns the http.Handler (exposed for tests).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/plain")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", s.handleProxy)
	return mux
}

func (s *Server) Addr() string {
	return fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	_, _, up, source := s.classify(r)
	if err := s.tr.Handle(w, r, up, source, s.rec, s.client, s.cfg.MaxBodyBytes); err != nil {
		log.Printf("goxray: proxy %s %s: %v", r.Method, r.URL.Path, err)
	}
}

// classify maps a request to (provider, transport, upstream URL, source slug).
// It strips a leading source slug from the path (see splitSourceSlug) so the
// upstream sees the real API path. MVP: always Anthropic over HTTP. This is the
// seam where OpenAI/Codex path rules and WS detection will slot in.
func (s *Server) classify(r *http.Request) (model.Provider, model.Transport, *url.URL, string) {
	source, apiPath := splitSourceSlug(r.URL.Path)
	up := *s.cfg.Upstream
	up.Path = apiPath
	up.RawQuery = r.URL.RawQuery
	return model.ProviderAnthropic, model.TransportHTTP, &up, source
}

// apiRoots are the Anthropic URL namespaces. A leading path segment that is not
// one of these is treated as a caller-declared source slug and stripped.
var apiRoots = map[string]bool{"v1": true, "api": true}

// splitSourceSlug separates an optional leading source slug from the API path.
//
//	/v1/messages            → ("",       "/v1/messages")   // no slug
//	/proj-a/v1/messages     → ("proj-a", "/v1/messages")   // slug stripped
//	/api/hello              → ("",       "/api/hello")
//
// The heuristic — first segment ∉ {v1, api} ⇒ slug — is unambiguous because
// Claude Code only ever calls /v1/* and /api/*.
func splitSourceSlug(path string) (source, apiPath string) {
	trimmed := strings.TrimPrefix(path, "/")
	first := trimmed
	rest := "/"
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		first, rest = trimmed[:i], trimmed[i:]
	}
	if first == "" || apiRoots[first] {
		return "", path
	}
	return first, rest
}

// isLoopback is a guard used in tests/config; kept here beside the bind choice.
func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

var _ = isLoopback // reserved for future non-loopback auth gating
