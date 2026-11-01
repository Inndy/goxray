package web

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/store"
	"go.inndy.tw/goxray/internal/wire"
)

type inferredTitle struct{ title, exchange string }

// Codex title threads have no target id on the wire. This disposable overlay
// infers a title only from a unique root session with matching prompt, source,
// and time window. It never changes the audit records or reads Codex's files.
func (s *server) overlayCodexTitles(rows []store.Session) {
	s.titleMu.Lock()
	defer s.titleMu.Unlock()
	if time.Since(s.titleRefresh) > 30*time.Second {
		s.titles = s.inferCodexTitles()
		s.titleRefresh = time.Now()
	}
	for i := range rows {
		if t, ok := s.titles[rows[i].SessionID]; ok && rows[i].Title == "" {
			rows[i].Title, rows[i].TitleSource, rows[i].TitleExchange = t.title, "proxy_inferred", t.exchange
		}
	}
}

func (s *server) recordedRequest(row model.Exchange) []byte {
	ex, err := s.rd.Manifest(row.ID)
	if err != nil {
		return nil
	}
	if ex.WS != nil {
		log, err := s.rd.Blob(ex.Req.SessionID, ex.WS.Log)
		if err != nil {
			return nil
		}
		t := wire.ReplayTurn(log, ex.WS.First, wsLast(ex))
		if t == nil || t.ID != ex.ID || t.Incomplete {
			return nil
		}
		return t.Request
	}
	b, err := s.rd.Blob(ex.Req.SessionID, ex.ReqRef)
	if err != nil {
		return nil
	}
	return wire.DecodeContent(ex.ReqHeaders["content-encoding"], b)
}

func userTexts(req []byte) []string {
	var r struct {
		Input []struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"input"`
	}
	if json.Unmarshal(req, &r) != nil {
		return nil
	}
	var texts []string
	for _, item := range r.Input {
		if item.Role != "user" || (item.Type != "message" && item.Type != "") {
			continue
		}
		var text string
		if json.Unmarshal(item.Content, &text) == nil {
			texts = append(texts, text)
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(item.Content, &parts) == nil {
			for _, p := range parts {
				if p.Type == "input_text" {
					texts = append(texts, p.Text)
				}
			}
		}
	}
	return texts
}

func titlePromptMatches(titlePrompt, user string) bool {
	if !strings.HasPrefix(titlePrompt, "Generate a concise, single-line task title ") {
		return false
	}
	i := strings.Index(titlePrompt, "\n\nUser prompt:\n")
	if i < 0 {
		return false
	}
	i += len("\n\nUser prompt:\n")
	budget := 960 - i
	if budget <= 0 {
		return false
	}
	user = strings.TrimSpace(user)
	if user == "" {
		return false
	}
	if len(user) > budget {
		end := budget
		for !utf8.RuneStart(user[end]) {
			end--
		}
		user = user[:end]
	}
	return titlePrompt[i:] == user
}

func (s *server) inferCodexTitles() map[string]inferredTitle {
	out := map[string]inferredTitle{}
	titles, err := s.rd.Query(store.Filter{IncludeNoise: true, TitleOnly: true, Limit: 1000})
	if err != nil {
		return out
	}
	requests := map[string][]string{}
	texts := func(c model.Exchange) []string {
		if v, ok := requests[c.ID]; ok {
			return v
		}
		v := userTexts(s.recordedRequest(c))
		requests[c.ID] = v
		return v
	}
	for _, title := range titles {
		if title.Provider != model.ProviderOpenAI || !title.IsNoise || title.Res.Title == "" {
			continue
		}
		prompts := texts(title)
		// Title generation and the first model turn can start concurrently.
		candidates, err := s.rd.Query(store.Filter{Source: title.Source, Since: title.StartedAt.Add(-5 * time.Minute).UnixMilli(), Until: title.StartedAt.Add(5 * time.Minute).UnixMilli(), Limit: 512})
		if err != nil || len(candidates) >= 512 {
			continue
		}
		matches := map[string]bool{}
		roots := map[string]model.Exchange{}
		for _, c := range candidates {
			if c.Provider != model.ProviderOpenAI || c.IsNoise || c.Req.IsSubagent || (c.Req.Thread != "" && c.Req.Thread != c.Req.SessionID) || c.Source != title.Source || c.Req.SessionID == "" || c.Req.SessionID == title.Req.SessionID || c.StartedAt.After(title.StartedAt.Add(5*time.Minute)) || c.StartedAt.Before(title.StartedAt.Add(-5*time.Minute)) {
				continue
			}
			if old, ok := roots[c.Req.SessionID]; !ok || c.StartedAt.Before(old.StartedAt) {
				roots[c.Req.SessionID] = c
			}
		}
		for _, c := range roots {
			for _, text := range texts(c) {
				for _, prompt := range prompts {
					if titlePromptMatches(prompt, text) {
						matches[c.Req.SessionID] = true
					}
				}
			}
		}
		if len(matches) == 1 {
			for sid := range matches {
				if _, exists := out[sid]; !exists {
					out[sid] = inferredTitle{title.Res.Title, title.ID}
				}
			}
		}
	}
	return out
}
