package main

import (
	"encoding/json"
	"fmt"
	"sort"
)

// cmdContent walks the largest requests and attributes raw bytes to JSON
// structure, to see what is actually taking the space (and what is
// incompressible).
func cmdContent(root string) {
	ms := loadManifests(root)
	// pick the largest request
	var best []byte
	var bestM Manifest
	for _, m := range ms {
		if m.ReqRef == "" {
			continue
		}
		raw, _, err := readBlob(root, m.ReqRef)
		if err != nil {
			continue
		}
		if len(raw) > len(best) {
			best, bestM = raw, m
		}
	}
	fmt.Printf("largest request: %s session=%s %.2f MiB msgs=%d tools=%d\n\n",
		bestM.ID, bestM.Req.SessionID, mib(int64(len(best))), bestM.Req.MsgCount, bestM.Req.ToolCount)

	var body map[string]json.RawMessage
	if err := json.Unmarshal(best, &body); err != nil {
		fmt.Println("unmarshal:", err)
		return
	}
	type kv struct {
		k string
		n int
	}
	var top []kv
	for k, v := range body {
		top = append(top, kv{k, len(v)})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].n > top[j].n })
	fmt.Println("top-level fields by bytes:")
	for _, e := range top {
		fmt.Printf("  %-16s %9d  %5.1f%%\n", e.k, e.n, 100*float64(e.n)/float64(len(best)))
	}

	// attribute messages[] by content block type
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(body["messages"], &msgs); err != nil {
		fmt.Println("messages:", err)
		return
	}
	byType := map[string]int{}
	byField := map[string]int{}
	nblocks := 0
	for _, m := range msgs {
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(m.Content, &blocks) != nil {
			byType["(string content) "+m.Role] += len(m.Content)
			continue
		}
		for _, b := range blocks {
			nblocks++
			t := "?"
			if raw, ok := b["type"]; ok {
				_ = json.Unmarshal(raw, &t)
			}
			total := 0
			for k, v := range b {
				total += len(k) + len(v) + 4
				byField[t+"."+k] += len(v)
			}
			byType[m.Role+"/"+t] += total
		}
	}
	fmt.Printf("\nmessages: %d, blocks: %d\nbytes by role/type:\n", len(msgs), nblocks)
	var tt []kv
	for k, v := range byType {
		tt = append(tt, kv{k, v})
	}
	sort.Slice(tt, func(i, j int) bool { return tt[i].n > tt[j].n })
	for _, e := range tt {
		fmt.Printf("  %-34s %9d  %5.1f%%\n", e.k, e.n, 100*float64(e.n)/float64(len(best)))
	}

	fmt.Println("\nbytes by block field (top 15):")
	var ff []kv
	for k, v := range byField {
		ff = append(ff, kv{k, v})
	}
	sort.Slice(ff, func(i, j int) bool { return ff[i].n > ff[j].n })
	for i, e := range ff {
		if i >= 15 {
			break
		}
		fmt.Printf("  %-34s %9d  %5.1f%%\n", e.k, e.n, 100*float64(e.n)/float64(len(best)))
	}
}
