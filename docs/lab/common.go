package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

type Manifest struct {
	ID        string            `json:"id"`
	State     string            `json:"state"`
	URL       string            `json:"url"`
	StartedAt time.Time         `json:"started_at"`
	Status    int               `json:"status"`
	IsNoise   bool              `json:"is_noise"`
	ReqRef    string            `json:"req_ref"`
	ResRef    string            `json:"res_ref"`
	ResHead   map[string]string `json:"res_headers"`
	Req       struct {
		Model     string `json:"model"`
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
		MsgCount  int    `json:"msg_count"`
		ToolCount int    `json:"tool_count"`
		Subagent  bool   `json:"is_subagent"`
	} `json:"req"`
}

func shardPath(dir, id, ext string) string {
	shard := id
	if len(id) >= 2 {
		shard = id[len(id)-2:]
	}
	return filepath.Join(dir, shard, id+ext)
}

func loadManifests(root string) []Manifest {
	var out []Manifest
	dir := filepath.Join(root, "manifests")
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var m Manifest
		if json.Unmarshal(b, &m) != nil {
			return nil
		}
		out = append(out, m)
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

var gdec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(1<<31))

// readBlob returns (raw, onDiskCompressedSize).
func readBlob(root, ref string) ([]byte, int, error) {
	p := shardPath(filepath.Join(root, "blobs"), ref, ".zst")
	comp, err := os.ReadFile(p)
	if err != nil {
		return nil, 0, err
	}
	raw, err := gdec.DecodeAll(comp, nil)
	if err != nil {
		return nil, len(comp), err
	}
	return raw, len(comp), nil
}

func mib(n int64) float64 { return float64(n) / (1 << 20) }
