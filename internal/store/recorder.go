package store

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/pricing"
	"go.inndy.tw/goxray/internal/wire"
)

// Observer is notified after each durability barrier commits. It exists for
// live read-side consumers (the dashboard); implementations MUST NOT block and
// MUST NOT panic — they run on the request goroutine, and the fail-closed
// guarantee has already been satisfied by the time they are called, so an
// observer can neither strengthen nor weaken it.
type Observer interface {
	OnExchange(ex model.Exchange)
}

// Recorder orchestrates the fail-closed commit sequence. The two Persist calls
// are the durability barriers: PersistRequest before forwarding, PersistResponse
// before releasing the terminal SSE frame. Only the SQLite Upsert is async.
type Recorder struct {
	blobs     BlobStore
	manifests ManifestStore
	idx       *Index
	parser    wire.WireParser
	provider  model.Provider

	obs  atomic.Pointer[Observer]
	jobs chan model.Exchange
	wg   sync.WaitGroup

	// mu orders enqueueIndex against Close: a handler surviving the shutdown
	// deadline must not send on the closed jobs channel (panic). Guarded adds
	// also make wg.Wait sound — every Add happens under RLock before closed is
	// set, so no Add can race the Wait.
	mu     sync.RWMutex
	closed bool
}

// SetObserver installs (or with nil, clears) the live observer. Safe to call at
// any time; the read on the hot path is a single atomic load.
func (r *Recorder) SetObserver(o Observer) {
	if o == nil {
		r.obs.Store(nil)
		return
	}
	r.obs.Store(&o)
}

// notify fires the observer, isolating the hot path from a misbehaving one.
func (r *Recorder) notify(ex *model.Exchange) {
	p := r.obs.Load()
	if p == nil {
		return
	}
	defer func() {
		if v := recover(); v != nil {
			logf("observer panic on %s: %v", ex.ID, v)
		}
	}()
	(*p).OnExchange(*ex)
}

// NewRecorder wires the durable stores together and starts the async index
// worker pool. Callers must Close it before closing the Index.
func NewRecorder(blobs BlobStore, manifests ManifestStore, idx *Index, provider model.Provider, workers int) *Recorder {
	if workers < 1 {
		workers = 1
	}
	r := &Recorder{
		blobs:     blobs,
		manifests: manifests,
		idx:       idx,
		parser:    wire.For(provider),
		provider:  provider,
		jobs:      make(chan model.Exchange, 256),
	}
	for n := 0; n < workers; n++ {
		r.wg.Add(1)
		go r.indexWorker()
	}
	return r
}

func (r *Recorder) indexWorker() {
	defer r.wg.Done()
	for ex := range r.jobs {
		if err := r.idx.Upsert(ex); err != nil {
			// Non-fatal: the manifest is durable; startup Reconcile recovers it.
			logf("index upsert %s: %v", ex.ID, err)
		}
	}
}

// NewExchange mints an inflight exchange from the request line + headers.
func (r *Recorder) NewExchange(method, rawURL string, h http.Header) *model.Exchange {
	return &model.Exchange{
		ID:         uuid.Must(uuid.NewV7()).String(),
		Provider:   r.provider,
		CaptureVer: model.CaptureVersion,
		State:      model.StateInflight,
		Method:     method,
		URL:        rawURL,
		StartedAt:  time.Now().UTC(),
		ReqHeaders: curateReqHeaders(h),
	}
}

// PersistRequest parses request metadata, stores the raw body blob, and writes
// the inflight manifest — all fsync'd. A non-nil error means the caller must
// NOT forward and must return 502 (fail-closed: no unlogged traffic).
func (r *Recorder) PersistRequest(ex *model.Exchange, rawReq []byte) error {
	path := strings.SplitN(ex.URL, "?", 2)[0]
	ex.IsNoise = r.parser.IsNoise(ex.Method, path, nil, rawReq)
	ex.Req = r.parser.ParseRequest(nil, rawReq)

	ref, err := r.blobs.Put(rawReq)
	if err != nil {
		return err
	}
	ex.ReqRef = ref
	if err := r.manifests.Write(ex); err != nil {
		return err
	}
	r.notify(ex)
	return nil
}

// PersistResponse parses response metadata, stores the raw response blob, and
// writes the final manifest (superseding the inflight one) — all fsync'd. The
// caller sets ex.State/Status/ResHeaders before calling. A non-nil error means
// the caller must abort the client connection (fail-closed). On the happy path
// ex.State should be StateComplete; error paths pass a terminal state and a
// possibly-partial rawRes, which is still recorded.
func (r *Recorder) PersistResponse(ex *model.Exchange, rawRes []byte) error {
	if ex.State == "" || ex.State == model.StateInflight {
		ex.State = model.StateComplete
	}
	ex.EndedAt = time.Now().UTC()

	ct := ""
	if ex.ResHeaders != nil {
		ct = ex.ResHeaders["content-type"]
	}
	meta, ok := r.parser.ParseResponse(nil, rawRes, ct)
	meta.Cost = pricing.Price(meta, ex.Req.Model)
	ex.Res = meta
	ex.ParseOK = ok

	ref, err := r.blobs.Put(rawRes)
	if err != nil {
		// The blob store failed but the manifests may still be writable: record
		// the terminal persist_failed state rather than leaving the inflight
		// manifest to masquerade as a crash. Parsed metadata (usage, model) is
		// kept — it was extracted before the blob was lost. Best-effort: if this
		// write fails too, the manifest simply stays inflight.
		ex.State = model.StatePersistFailed
		if werr := r.manifests.Write(ex); werr != nil {
			logf("persist_failed manifest %s: %v", ex.ID, werr)
		} else {
			r.enqueueIndex(*ex)
			r.notify(ex)
		}
		return err
	}
	ex.ResRef = ref
	if err := r.manifests.Write(ex); err != nil {
		return err
	}
	r.enqueueIndex(*ex)
	r.notify(ex)
	return nil
}

// enqueueIndex hands the exchange to the async index pool without blocking the
// request goroutine; if the queue is saturated it upserts in a tracked overflow
// goroutine as a fallback (still off the terminal-frame release, which already
// happened).
func (r *Recorder) enqueueIndex(ex model.Exchange) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		// The manifest is already durable; only the projection row is lost, and
		// only because a handler outlived the shutdown deadline.
		logf("index enqueue after close %s: row skipped (manifest durable)", ex.ID)
		return
	}
	select {
	case r.jobs <- ex:
	default:
		r.wg.Go(func() {
			if err := r.idx.Upsert(ex); err != nil {
				logf("index upsert (overflow) %s: %v", ex.ID, err)
			}
		})
	}
}

// Close drains the async index queue, including overflow goroutines. Call
// before Index.Close.
func (r *Recorder) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.jobs)
	r.mu.Unlock()
	r.wg.Wait()
}

var curatedReqKeys = []string{"anthropic-version", "anthropic-beta", "user-agent", "content-type"}

func curateReqHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for _, k := range curatedReqKeys {
		if v := h.Get(k); v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
