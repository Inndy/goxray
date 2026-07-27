package store

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/pricing"
	"go.inndy.tw/goxray/internal/wire"
)

// Recorder orchestrates the fail-closed commit sequence. The two Persist calls
// are the durability barriers: PersistRequest before forwarding, PersistResponse
// before releasing the terminal SSE frame. Only the SQLite Upsert is async.
type Recorder struct {
	blobs     BlobStore
	manifests ManifestStore
	idx       *Index
	parser    wire.WireParser
	provider  model.Provider

	jobs chan model.Exchange
	wg   sync.WaitGroup
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
	return r.manifests.Write(ex)
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
	meta.Cost = pricing.Calc(meta.Usage, ex.Req.Model)
	ex.Res = meta
	ex.ParseOK = ok

	ref, err := r.blobs.Put(rawRes)
	if err != nil {
		return err
	}
	ex.ResRef = ref
	if err := r.manifests.Write(ex); err != nil {
		return err
	}
	r.enqueueIndex(*ex)
	return nil
}

// enqueueIndex hands the exchange to the async index pool without blocking the
// request goroutine; if the queue is saturated it upserts inline as a fallback
// (still off the terminal-frame release, which already happened).
func (r *Recorder) enqueueIndex(ex model.Exchange) {
	select {
	case r.jobs <- ex:
	default:
		go func() {
			if err := r.idx.Upsert(ex); err != nil {
				logf("index upsert (overflow) %s: %v", ex.ID, err)
			}
		}()
	}
}

// Close drains the async index queue. Call before Index.Close.
func (r *Recorder) Close() {
	close(r.jobs)
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
