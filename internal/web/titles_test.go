package web

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/store"
)

func requestWithUser(text string) []byte {
	b, _ := json.Marshal(map[string]any{"input": []any{map[string]any{"type": "message", "role": "user", "content": text}}})
	return b
}

func TestProxyTitleOverlay(t *testing.T) {
	for _, mode := range []string{"unique", "concurrent", "ambiguous", "other-source", "existing-title", "unreadable", "future", "expired", "subagent"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			root := model.Exchange{ID: "root", Provider: model.ProviderOpenAI, Source: "project", StartedAt: now.Add(-time.Second), ReqRef: "request", Req: model.ReqMeta{SessionID: "session"}}
			title := model.Exchange{ID: "title", Provider: model.ProviderOpenAI, Source: "project", StartedAt: now, IsNoise: true, ReqRef: "title-request", Req: model.ReqMeta{SessionID: "temporary"}, Res: model.ResMeta{Title: "Fix parser"}}
			rd := &fakeReader{rows: []model.Exchange{title, root}, blobs: map[string][]byte{
				"request":       requestWithUser("fix the parser"),
				"title-request": requestWithUser("Generate a concise, single-line task title of at most 36 characters.\n\nUser prompt:\nfix the parser"),
			}}
			rows := []store.Session{{SessionID: "session"}}
			switch mode {
			case "ambiguous":
				other := root
				other.ID = "other"
				other.Req.SessionID = "second-session"
				rd.rows = append(rd.rows, other)
			case "other-source":
				rd.rows[1].Source = "elsewhere"
			case "existing-title":
				rows[0].Title = "Recorded title"
			case "unreadable":
				delete(rd.blobs, "request")
			case "future":
				rd.rows[1].StartedAt = now.Add(6 * time.Minute)
			case "concurrent":
				rd.rows[1].StartedAt = now.Add(time.Second)
			case "expired":
				rd.rows[1].StartedAt = now.Add(-6 * time.Minute)
			case "subagent":
				rd.rows[1].Req.IsSubagent = true
			}
			s := &server{rd: rd}
			s.overlayCodexTitles(rows)
			if mode == "unique" || mode == "concurrent" {
				if rows[0].Title != "Fix parser" || rows[0].TitleSource != "proxy_inferred" || rows[0].TitleExchange != "title" {
					t.Fatalf("missing inference: %+v", rows)
				}
			} else if mode == "existing-title" {
				if rows[0].Title != "Recorded title" {
					t.Fatal("overwrote recorded title")
				}
			} else if rows[0].Title != "" {
				t.Fatalf("unsafe association: %+v", rows)
			}
		})
	}
}

func TestTitlePromptUnicodeTruncation(t *testing.T) {
	prefix := "Generate a concise, single-line task title of at most 36 characters.\n\nUser prompt:\n"
	user := strings.Repeat("測試", 500)
	short := user
	for len(prefix)+len(short) > 960 {
		short = short[:len(short)-3]
	}
	if !titlePromptMatches(prefix+short, user) {
		t.Fatal("bounded Unicode prompt did not match")
	}
	if titlePromptMatches(prefix+"fix", "fix a different task") {
		t.Fatal("accepted an arbitrary prefix")
	}
	if titlePromptMatches("Recent conversation messages:\nfix", "fix") {
		t.Fatal("accepted rename suggestion")
	}
}
