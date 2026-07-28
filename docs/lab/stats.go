package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

func cmdStats(root string) {
	ms := loadManifests(root)
	fmt.Printf("manifests: %d\n", len(ms))

	type sess struct {
		id      string
		n       int
		rawReq  int64
		rawRes  int64
		diskReq int64
		diskRes int64
		maxReq  int
		models  map[string]int
	}
	sessions := map[string]*sess{}
	var totRawReq, totRawRes, totDiskReq, totDiskRes int64
	var missing, noise, subagent, emptySid int
	states := map[string]int{}
	ctypes := map[string]int{}

	for _, m := range ms {
		states[m.State]++
		if m.IsNoise {
			noise++
		}
		if m.Req.Subagent {
			subagent++
		}
		if m.Req.SessionID == "" {
			emptySid++
		}
		ct := m.ResHead["content-type"]
		if i := strings.Index(ct, ";"); i > 0 {
			ct = ct[:i]
		}
		ctypes[ct]++

		s := sessions[m.Req.SessionID]
		if s == nil {
			s = &sess{id: m.Req.SessionID, models: map[string]int{}}
			sessions[m.Req.SessionID] = s
		}
		s.n++
		s.models[m.Req.Model]++

		if m.ReqRef != "" {
			raw, disk, err := readBlob(root, m.ReqRef)
			if err != nil {
				missing++
			} else {
				s.rawReq += int64(len(raw))
				s.diskReq += int64(disk)
				totRawReq += int64(len(raw))
				totDiskReq += int64(disk)
				if len(raw) > s.maxReq {
					s.maxReq = len(raw)
				}
			}
		}
		if m.ResRef != "" {
			raw, disk, err := readBlob(root, m.ResRef)
			if err != nil {
				missing++
			} else {
				s.rawRes += int64(len(raw))
				s.diskRes += int64(disk)
				totRawRes += int64(len(raw))
				totDiskRes += int64(disk)
			}
		}
	}

	fmt.Printf("states: %v\nnoise=%d subagent=%d empty_session_id=%d missing_blobs=%d\n", states, noise, subagent, emptySid, missing)
	fmt.Printf("res content-types: %v\n", ctypes)
	fmt.Printf("\nRAW   req=%.1f MiB  res=%.1f MiB  total=%.1f MiB\n", mib(totRawReq), mib(totRawRes), mib(totRawReq+totRawRes))
	fmt.Printf("DISK  req=%.1f MiB  res=%.1f MiB  total=%.1f MiB   (ratio req %.2fx res %.2fx)\n",
		mib(totDiskReq), mib(totDiskRes), mib(totDiskReq+totDiskRes),
		float64(totRawReq)/float64(totDiskReq), float64(totRawRes)/float64(totDiskRes))

	var list []*sess
	for _, s := range sessions {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].diskReq+list[i].diskRes > list[j].diskReq+list[j].diskRes })
	fmt.Printf("\nsessions: %d\n", len(list))
	fmt.Printf("%-38s %5s %10s %10s %10s %10s %9s\n", "session", "turns", "rawReq", "diskReq", "rawRes", "diskRes", "maxReqKB")
	for i, s := range list {
		if i >= 15 {
			fmt.Printf("... %d more\n", len(list)-i)
			break
		}
		id := s.id
		if id == "" {
			id = "(empty)"
		}
		fmt.Printf("%-38s %5d %9.1fM %9.1fM %9.1fM %9.1fM %9d\n", id, s.n, mib(s.rawReq), mib(s.diskReq), mib(s.rawRes), mib(s.diskRes), s.maxReq/1024)
	}

	// turn-count histogram
	buckets := map[string]int{}
	for _, s := range list {
		switch {
		case s.n == 1:
			buckets["1"]++
		case s.n <= 5:
			buckets["2-5"]++
		case s.n <= 20:
			buckets["6-20"]++
		case s.n <= 50:
			buckets["21-50"]++
		default:
			buckets["50+"]++
		}
	}
	fmt.Printf("\nturns/session histogram: %v\n", buckets)
	_ = os.Stdout
}
