package web

import (
	"sort"
	"sync"
	"time"

	"go.inndy.tw/goxray/internal/model"
)

// inflightTTL bounds how long an exchange may sit in the inflight set. The
// recorder cannot notify when a response fails to persist (it returns the error
// before reaching the observer), so an entry can be orphaned. Expiring it keeps
// the set from growing without bound; the proxy's own idle deadline is 5 minutes,
// so anything older than this is definitely finished one way or another.
const inflightTTL = 10 * time.Minute

// settledTTL is how long a finished exchange is kept after its final frame. The
// recorder announces a commit before its index row is written (on another
// goroutine, behind a queue), so for a moment a finished turn is neither inflight
// nor indexed; an aggregate computed then would miss it, with nothing to make it
// look again. Keeping it lets the overlays count it until the index has it.
const settledTTL = 2 * time.Minute

const settledPruneEvery = 10 * time.Second

// clientBuffer is how many events a slow dashboard client may fall behind
// before it starts losing them. Dropping is deliberate: the recording path must
// never wait on a browser. A client that misses events refetches the list on
// reconnect, so the durable store stays the source of truth.
const clientBuffer = 64

// Hub fans recorded exchanges out to connected dashboards and tracks the ones
// still in flight. It implements store.Observer.
type Hub struct {
	mu       sync.RWMutex
	clients  map[chan model.Exchange]struct{}
	inflight map[string]model.Exchange
	settled  map[string]settledExchange
	pruned   time.Time
}

type settledExchange struct {
	ex model.Exchange
	at time.Time
}

func NewHub() *Hub {
	return &Hub{
		clients:  map[chan model.Exchange]struct{}{},
		inflight: map[string]model.Exchange{},
		settled:  map[string]settledExchange{},
	}
}

// OnExchange receives every committed exchange. It must never block the caller —
// it runs on the proxy's request goroutine.
func (h *Hub) OnExchange(ex model.Exchange) {
	h.mu.Lock()
	if ex.State == model.StateInflight {
		h.inflight[ex.ID] = ex
	} else {
		delete(h.inflight, ex.ID)
		now := time.Now()
		h.settled[ex.ID] = settledExchange{ex, now}
		// Pruned here as well as on read: a proxy nobody is watching never
		// reads, and the set would otherwise keep every exchange it records.
		// At most every settledPruneEvery, since this is the request goroutine.
		if now.Sub(h.pruned) > settledPruneEvery {
			h.pruned = now
			for id, s := range h.settled {
				if now.Sub(s.at) > settledTTL {
					delete(h.settled, id)
				}
			}
		}
	}
	for ch := range h.clients {
		select {
		case ch <- ex:
		default: // client is behind; drop rather than stall the proxy
		}
	}
	h.mu.Unlock()
}

// Inflight returns the exchanges currently mid-flight, newest first, evicting
// any that have outlived their TTL.
func (h *Hub) Inflight() []model.Exchange {
	cutoff := time.Now().Add(-inflightTTL)
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]model.Exchange, 0, len(h.inflight))
	for id, ex := range h.inflight {
		if ex.StartedAt.Before(cutoff) {
			delete(h.inflight, id)
			continue
		}
		out = append(out, ex)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

// Snapshot returns the inflight exchanges and those that finished within
// settledTTL, under one lock: an exchange finishing between two separate reads
// would be in both, and counted twice. Some settled ones may already be indexed;
// callers check.
func (h *Hub) Snapshot() (inflight, settled []model.Exchange) {
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, ex := range h.inflight {
		if now.Sub(ex.StartedAt) > inflightTTL {
			delete(h.inflight, id)
			continue
		}
		inflight = append(inflight, ex)
	}
	for id, s := range h.settled {
		if now.Sub(s.at) > settledTTL {
			delete(h.settled, id)
			continue
		}
		settled = append(settled, s.ex)
	}
	return inflight, settled
}

func (h *Hub) subscribe() (<-chan model.Exchange, func()) {
	ch := make(chan model.Exchange, clientBuffer)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.clients, ch)
		h.mu.Unlock()
	}
}
