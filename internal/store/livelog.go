package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"

	"go.inndy.tw/goxray/internal/wire"
)

// A live connection log is blobs/live/<ref>.gxl: a run of segments, each a
// little-endian uint32 length followed by one complete zstd frame. A segment is
// appended and fsync'd at every durability barrier, so whatever it holds is
// either wholly on disk or a torn tail. When the connection ends the log is
// sealed into an ordinary <ref>.zst blob. See docs/codex.md.

const liveDir = "live"

// Format limits. A .gxl is stored bytes, so every length in it is untrusted;
// these turn damage into a reported error instead of a huge allocation.
const (
	// segMaxRaw bounds one decoded segment. A segment is at most one client
	// frame — bounded by --max-body-mb — or one turn's worth of events between
	// barriers, plus record framing.
	segMaxRaw = 256 << 20
	// segMaxComp bounds one compressed segment; zstd never expands much.
	segMaxComp = segMaxRaw + segMaxRaw/16
)

// logSoftLimit is where the proxy rotates a connection between turns, and
// logHardLimit where it aborts one mid-turn. Close and end records are exempt
// from the hard limit, so a connection can always be finished, and logSlack
// bounds what they may add. All of it sits under archiveMaxBody, which a sealed
// log has to fit in to ever be archived. Variables only so a test can lower
// them.
var (
	logSoftLimit int64 = 128 << 20
	logHardLimit int64 = 192 << 20
	logSlack     int64 = 1 << 20
)

// logMaxRaw is the most a log can hold: anything larger was not written by
// goxray.
func logMaxRaw() int64 { return logHardLimit + logSlack }

func livePath(blobsDir, ref string) string {
	return filepath.Join(blobsDir, liveDir, ref+".gxl")
}

// liveWriter appends segments to an open .gxl.
type liveWriter struct {
	f   *os.File
	enc *zstd.Encoder
	// err is the first append failure. A failed append leaves the file in an
	// unknown state — the write may have landed and only the fsync failed — so
	// nothing is appended after one: a retry could duplicate records, and
	// anything written behind a partial frame would read back as a torn tail.
	err error
}

func createLive(blobsDir, ref string) (*liveWriter, error) {
	dir := filepath.Join(blobsDir, liveDir)
	if err := ensureDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(livePath(blobsDir, ref), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := fsyncDir(dir); err != nil {
		f.Close()
		return nil, err
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		f.Close()
		return nil, err
	}
	return &liveWriter{f: f, enc: enc}, nil
}

// append writes one segment and makes it durable. An empty segment is a
// no-op: a barrier with nothing new to record has nothing to sync.
func (w *liveWriter) append(raw []byte) error {
	if w.err != nil {
		return w.err
	}
	if len(raw) == 0 {
		return nil
	}
	frame := w.enc.EncodeAll(raw, make([]byte, 4, 4+len(raw)/2))
	binary.LittleEndian.PutUint32(frame[:4], uint32(len(frame)-4))
	_, err := w.f.Write(frame)
	if err == nil {
		err = w.f.Sync()
	}
	if err != nil {
		w.err = fmt.Errorf("live log append: %w", err)
	}
	return w.err
}

func (w *liveWriter) close() error {
	w.enc.Close()
	return w.f.Close()
}

var errLiveCorrupt = errors.New("live log corrupt")

// eachSegment walks a .gxl's decoded segments in order. Only a final segment
// whose declared length runs past end of file is a torn tail, and is skipped;
// every other inconsistency is corruption.
func eachSegment(r io.Reader, fn func(raw []byte) error) error {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(segMaxRaw), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return err
	}
	defer dec.Close()
	var hdr [4]byte
	for i := 0; ; i++ {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil // clean end, or a torn length prefix
			}
			return err
		}
		n := binary.LittleEndian.Uint32(hdr[:])
		if n == 0 || n > segMaxComp {
			return fmt.Errorf("%w: segment %d claims %d bytes", errLiveCorrupt, i, n)
		}
		comp := make([]byte, n)
		if _, err := io.ReadFull(r, comp); err != nil {
			if err == io.ErrUnexpectedEOF {
				return nil // torn tail
			}
			return err
		}
		raw, err := dec.DecodeAll(comp, nil)
		if err != nil {
			return fmt.Errorf("%w: segment %d: %v", errLiveCorrupt, i, err)
		}
		if err := fn(raw); err != nil {
			return err
		}
	}
}

// readLive returns a .gxl's decoded content, verified: every record parses and
// record numbers run 1, 2, 3… without a gap.
func readLive(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []byte
	var c recordChecker
	err = eachSegment(f, func(raw []byte) error {
		if int64(len(out)+len(raw)) > logMaxRaw() {
			return fmt.Errorf("%w: over %d bytes decoded", errLiveCorrupt, logMaxRaw())
		}
		if err := c.segment(raw); err != nil {
			return err
		}
		out = append(out, raw...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// checkRecords verifies a whole decoded log's framing.
func checkRecords(log []byte) error {
	var c recordChecker
	return c.segment(log)
}

// recordChecker verifies a log's framing a segment at a time: every record is a
// whole line that parses, and record numbers run 1, 2, 3… with no gap and no
// repeat. A segment is written whole or not at all, so a line that does not
// parse cannot be a torn write here.
type recordChecker struct{ last int64 }

func (c *recordChecker) segment(raw []byte) error {
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		return fmt.Errorf("%w: unterminated record after n=%d", errLiveCorrupt, c.last)
	}
	for len(raw) > 0 {
		i := bytes.IndexByte(raw, '\n')
		line := raw[:i]
		raw = raw[i+1:]
		var r wire.Record
		if json.Unmarshal(line, &r) != nil {
			return fmt.Errorf("%w: unparseable record after n=%d", errLiveCorrupt, c.last)
		}
		if r.N != c.last+1 {
			return fmt.Errorf("%w: record %d where %d was expected", errLiveCorrupt, r.N, c.last+1)
		}
		c.last = r.N
	}
	return nil
}

// sealLive streams a .gxl into the ordinary blob for ref, installed by atomic
// rename, and returns the decoded size. The .gxl itself is left for the caller
// to unlink once every manifest that depends on it is settled.
func sealLive(blobsDir, ref string) (int64, error) {
	src, err := os.Open(livePath(blobsDir, ref))
	if err != nil {
		return 0, err
	}
	defer src.Close()

	final := shardPath(blobsDir, ref, ".zst")
	dir := filepath.Dir(final)
	if err := ensureDir(dir); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	enc, err := zstd.NewWriter(tmp, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		tmp.Close()
		return 0, err
	}
	// Verified as it streams: once the .gxl is gone the blob is what every turn
	// range is read against, so a log that would not read back must never be
	// installed as one.
	var total int64
	var c recordChecker
	err = eachSegment(src, func(raw []byte) error {
		total += int64(len(raw))
		if total > logMaxRaw() {
			return fmt.Errorf("%w: over %d bytes decoded", errLiveCorrupt, logMaxRaw())
		}
		if err := c.segment(raw); err != nil {
			return err
		}
		_, err := enc.Write(raw)
		return err
	})
	if err == nil {
		err = enc.Close()
	} else {
		enc.Close()
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return 0, err
	}
	// Close src before the rename for Windows' sake: nothing here renames onto
	// it, but the caller unlinks it next.
	src.Close()
	if err := renameOver(tmpName, final); err != nil {
		return 0, err
	}
	return total, fsyncDir(dir)
}

func liveExists(blobsDir, ref string) bool {
	if ref == "" || !isPlainToken(ref) {
		return false
	}
	_, err := os.Stat(livePath(blobsDir, ref))
	return err == nil
}
