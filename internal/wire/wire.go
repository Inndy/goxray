// Package wire holds provider-specific, best-effort metadata extraction.
//
// A WireParser never fails fatally: on an unrecognized shape it returns
// zero-valued metadata, and the raw blob on disk remains the authoritative
// record. The headers parameter is present on every method so future providers
// (OpenAI/Codex) can route on headers (e.g. chatgpt-account-id) without a
// signature change.
package wire

import (
	"net/http"

	"go.inndy.tw/goxray/internal/model"
)

// WireParser extracts metadata from a captured request/response.
type WireParser interface {
	// IsNoise classifies low-signal traffic (token pre-counts, quota probes).
	// Noise is still recorded (compliance) but flagged so queries can hide it.
	IsNoise(method, path string, h http.Header, body []byte) bool
	// ParseRequest extracts request-side metadata from the raw body.
	ParseRequest(h http.Header, body []byte) model.ReqMeta
	// ParseResponse extracts response-side metadata from the raw captured bytes.
	// ok is false when the payload could not be parsed into the expected shape.
	ParseResponse(h http.Header, raw []byte, contentType string) (meta model.ResMeta, ok bool)
}

// registry maps a provider to its parser. One entry for MVP.
var registry = map[model.Provider]WireParser{
	model.ProviderAnthropic: Anthropic{},
}

// For returns the parser for a provider, defaulting to Anthropic.
func For(p model.Provider) WireParser {
	if wp, ok := registry[p]; ok {
		return wp
	}
	return Anthropic{}
}
