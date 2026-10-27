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
	"strings"

	"github.com/klauspost/compress/zstd"

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

// registry maps a provider to its parser.
var registry = map[model.Provider]WireParser{
	model.ProviderAnthropic: Anthropic{},
	model.ProviderOpenAI:    OpenAI{},
}

// For returns the parser for a provider, defaulting to Anthropic.
func For(p model.Provider) WireParser {
	if wp, ok := registry[p]; ok {
		return wp
	}
	return Anthropic{}
}

// maxDecoded bounds a decoded request body, so a small compressed body cannot
// expand without limit in the proxy or the dashboard. It matches the largest
// body an archive takes; nothing bigger is a request goxray could keep anyway.
const maxDecoded = 256 << 20

// DecodeContent removes a request's content coding for parsing and display.
// The recorded body keeps the coding — it is what crossed the wire — and every
// reader decodes a copy through here. Codex compresses with zstd under ChatGPT
// sign-in. Anything unknown or undecodable comes back unchanged.
func DecodeContent(encoding string, body []byte) []byte {
	if !strings.EqualFold(strings.TrimSpace(encoding), "zstd") {
		return body
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(maxDecoded), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return body
	}
	defer dec.Close()
	out, err := dec.DecodeAll(body, nil)
	if err != nil {
		return body
	}
	return out
}
