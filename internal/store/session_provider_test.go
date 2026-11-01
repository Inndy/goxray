package store

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.inndy.tw/goxray/internal/model"
)

func TestSessionProvidersAndTitleFilter(t *testing.T) {
	idx, _, _, err := OpenIndex(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i, p := range []model.Provider{model.ProviderOpenAI, model.ProviderAnthropic} {
		e := model.Exchange{ID: []string{"a", "b"}[i], Provider: p, StartedAt: now.Add(time.Duration(i) * time.Second), Req: model.ReqMeta{SessionID: "mixed"}, Res: model.ResMeta{Title: "title"}}
		if err := idx.Upsert(e); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := idx.Sessions(Filter{})
	if err != nil || len(rows) != 1 || !slices.Contains(rows[0].Providers, "openai") || !slices.Contains(rows[0].Providers, "anthropic") {
		t.Fatalf("providers: %+v, %v", rows, err)
	}
	entries, err := idx.Query(Filter{TitleOnly: true, IncludeNoise: true, Until: now.UnixMilli()})
	if err != nil || len(entries) != 1 || entries[0].ID != "a" {
		t.Fatalf("title filter: %+v, %v", entries, err)
	}
}
