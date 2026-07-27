// Package model holds the transport-blind data types shared across goxray.
//
// The central type is Exchange: one logical request→response pair, and the
// audit record for it. HTTP produces one Exchange per connection; a future
// WebSocket transport (Codex) would multiplex many Exchanges over one socket.
// Nothing downstream of the transport knows which.
package model

import "time"

// CaptureVersion is stamped into every manifest so a future format change is a
// clean migration rather than a guess. Bump when the on-disk contract changes.
const CaptureVersion = 1

// Provider identifies the upstream API family. MVP records only Anthropic.
type Provider string

const ProviderAnthropic Provider = "anthropic"

// Transport identifies how bytes flowed. MVP is HTTP only.
type Transport int

const (
	TransportHTTP Transport = iota
	TransportWS
)

// ExchangeState is the terminal (or inflight) disposition of an exchange. Every
// request that reaches the API is represented on disk as exactly one of these —
// inflight first, then a terminal state at commit or failure.
type ExchangeState string

const (
	StateInflight      ExchangeState = "inflight"       // req persisted, response not yet committed
	StateComplete      ExchangeState = "complete"       // clean upstream EOF, fully recorded
	StateClientAborted ExchangeState = "client_aborted" // client disconnected mid-stream
	StateUpstreamError ExchangeState = "upstream_error" // upstream RST/TLS error mid-stream
	StatePersistFailed ExchangeState = "persist_failed" // durable write of the response failed
	StateTimeout       ExchangeState = "timeout"        // upstream idle deadline fired
)

// Usage is the token accounting extracted from a response. Zero-valued when the
// response could not be parsed (schema drift) — the raw blob stays authoritative.
type Usage struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	CacheReadTokens   int `json:"cache_read_input_tokens"`
	CacheCreateTokens int `json:"cache_creation_input_tokens"`
}

// ReqMeta is best-effort metadata parsed from the request body.
type ReqMeta struct {
	Model      string `json:"model"`
	SessionID  string `json:"session_id"`
	Cwd        string `json:"cwd"`
	MsgCount   int    `json:"msg_count"`
	ToolCount  int    `json:"tool_count"`
	IsSubagent bool   `json:"is_subagent"`
}

// ResMeta is best-effort metadata parsed from the response.
type ResMeta struct {
	Usage      Usage   `json:"usage"`
	StopReason string  `json:"stop_reason"`
	Title      string  `json:"title,omitempty"`
	Cost       float64 `json:"cost"`
}

// Exchange is the audit record for one request→response pair. Serialized as the
// per-exchange manifest (the durable source of truth) and projected into SQLite.
// The manifest carries all parsed metadata so the index can be rebuilt from
// manifests alone — the raw blobs are never re-parsed for a rebuild.
type Exchange struct {
	ID         string        `json:"id"` // uuidv7 — time-sortable
	Provider   Provider      `json:"provider"`
	CaptureVer int           `json:"capture_ver"`
	State      ExchangeState `json:"state"`
	// Source is a caller-declared origin label, taken from a leading URL path
	// slug (ANTHROPIC_BASE_URL=http://host:port/<slug>). It lets one goxray
	// separate traffic by project / agent-group (single-user) or by user/tenant
	// (centralized). Explicit and trusted by construction — never inferred.
	Source    string    `json:"source,omitempty"`
	Method    string    `json:"method"`
	URL       string    `json:"url"` // path+query, auth params stripped
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Status    int       `json:"status"` // upstream HTTP status
	IsNoise   bool      `json:"is_noise"`
	ReqRef    string    `json:"req_ref"`
	ResRef    string    `json:"res_ref,omitempty"` // empty until complete

	ReqHeaders map[string]string `json:"req_headers,omitempty"` // curated
	ResHeaders map[string]string `json:"res_headers,omitempty"` // incl request-id, ratelimit

	Req     ReqMeta `json:"req"`
	Res     ResMeta `json:"res"`
	ParseOK bool    `json:"parse_ok"`
}

// ElapsedMS returns the wall-clock duration of the exchange in milliseconds.
func (e *Exchange) ElapsedMS() int64 {
	if e.EndedAt.IsZero() || e.StartedAt.IsZero() {
		return 0
	}
	return e.EndedAt.Sub(e.StartedAt).Milliseconds()
}

// RequestID returns the upstream request-id header if captured (audit correlation).
func (e *Exchange) RequestID() string {
	if e.ResHeaders == nil {
		return ""
	}
	return e.ResHeaders["request-id"]
}
