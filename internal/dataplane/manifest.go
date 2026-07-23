package dataplane

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/shulman33/krill/internal/sqlitewal"
)

// Manifest is THE stream manifest (E4): the single arbiter of an app
// stream's durable history. It lives in object storage and every mutation
// is a conditional PUT — the generation returned alongside it is the CAS
// token. Gateways (and hosts) are caches of it, never authorities.
type Manifest struct {
	App    string `json:"app"`
	Stream string `json:"stream"`
	// Parent links a PITR branch to the lineage it forked from (D4): this
	// stream's LSNs 1..Parent.LSN resolve through the parent chain.
	Parent *Branch `json:"parent,omitempty"`
	// CurEpoch is the sealed/current epoch — the spec's curEpoch variable.
	CurEpoch Epoch `json:"cur_epoch"`
	// HeadLSN counts frames accepted into this stream, branch-inclusive:
	// a branch created at parent LSN L starts with HeadLSN = L.
	HeadLSN uint64 `json:"head_lsn"`
	// PageSize is fixed by the first frames segment shipped.
	PageSize    int          `json:"page_size,omitempty"`
	Segments    []Segment    `json:"segments,omitempty"`
	Checkpoints []Checkpoint `json:"checkpoints,omitempty"`
	Seals       []Seal       `json:"seals,omitempty"`
	// Branches lists streams that forked from this one (informational, for
	// GC and the stream-status API; the branch's own Parent field is the
	// load-bearing link).
	Branches []Branch `json:"branches,omitempty"`
}

type Branch struct {
	Stream string `json:"stream"`
	LSN    uint64 `json:"lsn"`
}

// SegmentKind distinguishes ordinary frame segments from rebases.
type SegmentKind string

const (
	// SegFrames: the object holds WAL frames; replay applies them in order.
	SegFrames SegmentKind = "frames"
	// SegRebase: the object holds a complete database image that REPLACES
	// all prior state. Shipped when frame granularity was lost — the guest
	// checkpointed its WAL between polls (salt change with unshipped
	// frames). Durability is preserved; PITR granularity coarsens across
	// the jump. A rebase advances HeadLSN by exactly 1.
	SegRebase SegmentKind = "rebase"
)

// Segment records one shipped object: frames (FromLSN, ToLSN] of the
// logical stream, stamped with the epoch that shipped them (E2).
type Segment struct {
	Kind    SegmentKind `json:"kind"`
	Epoch   Epoch       `json:"epoch"`
	FromLSN uint64      `json:"from_lsn"` // exclusive: first frame is FromLSN+1
	ToLSN   uint64      `json:"to_lsn"`   // inclusive
	Key     string      `json:"key"`
	// Time is the HOST clock at ship time (guest clocks jump across resume
	// and are never trusted); resolves `restore --at-time`.
	Time time.Time `json:"time"`
}

// Checkpoint is a registered full-database image at an exact stream LSN —
// the E5 object. Unregistered checkpoint blobs are garbage by definition.
type Checkpoint struct {
	Epoch Epoch     `json:"epoch"`
	LSN   uint64    `json:"lsn"`
	Key   string    `json:"key"`
	Time  time.Time `json:"time"`
}

// Seal records an epoch takeover: everything at or below AtLSN belongs to
// predecessor epochs; Epoch owns the stream from here (E3's SEAL record).
type Seal struct {
	Epoch Epoch     `json:"epoch"`
	AtLSN uint64    `json:"at_lsn"`
	Time  time.Time `json:"time"`
}

func manifestKey(app, stream string) string {
	return fmt.Sprintf("apps/%s/%s/manifest.json", app, stream)
}

func segmentKey(app, stream string, from, to uint64, e Epoch) string {
	return fmt.Sprintf("apps/%s/%s/seg/%016d-%016d-%s", app, stream, from, to, e)
}

func checkpointKey(app, stream string, lsn uint64, e Epoch) string {
	return fmt.Sprintf("apps/%s/%s/ckpt/%016d-%s.db", app, stream, lsn, e)
}

func encodeManifest(m *Manifest) []byte {
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		panic("manifest is always marshalable: " + err.Error())
	}
	return b
}

func decodeManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("dataplane: corrupt manifest: %w", err)
	}
	return &m, nil
}

// --- segment object codec ---
//
//	KSEG1\n<json header>\n<frame bytes...>
//
// For SegFrames the body is [pgno u32be][commit u32be][page] per frame;
// for SegRebase the body is a raw database image.

const segMagic = "KSEG1\n"

type segHeader struct {
	App      string      `json:"app"`
	Stream   string      `json:"stream"`
	Kind     SegmentKind `json:"kind"`
	Epoch    Epoch       `json:"epoch"`
	FromLSN  uint64      `json:"from_lsn"`
	ToLSN    uint64      `json:"to_lsn"`
	PageSize int         `json:"page_size"`
}

func encodeSegment(h segHeader, frames []sqlitewal.Frame, rebaseImage []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString(segMagic)
	hb, _ := json.Marshal(h)
	buf.Write(hb)
	buf.WriteByte('\n')
	if h.Kind == SegRebase {
		buf.Write(rebaseImage)
		return buf.Bytes()
	}
	var u [4]byte
	for _, f := range frames {
		binary.BigEndian.PutUint32(u[:], f.Pgno)
		buf.Write(u[:])
		binary.BigEndian.PutUint32(u[:], f.Commit)
		buf.Write(u[:])
		buf.Write(f.Data)
	}
	return buf.Bytes()
}

// decodeSegment parses a segment object. For SegFrames it returns the
// frames; for SegRebase it returns the image as the second value.
func decodeSegment(b []byte) (segHeader, []sqlitewal.Frame, []byte, error) {
	var h segHeader
	if !bytes.HasPrefix(b, []byte(segMagic)) {
		return h, nil, nil, fmt.Errorf("dataplane: bad segment magic")
	}
	r := bufio.NewReader(bytes.NewReader(b[len(segMagic):]))
	line, err := r.ReadBytes('\n')
	if err != nil {
		return h, nil, nil, fmt.Errorf("dataplane: segment header: %w", err)
	}
	if err := json.Unmarshal(line, &h); err != nil {
		return h, nil, nil, fmt.Errorf("dataplane: segment header: %w", err)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return h, nil, nil, err
	}
	if h.Kind == SegRebase {
		return h, nil, body, nil
	}
	if h.PageSize < 512 {
		return h, nil, nil, fmt.Errorf("dataplane: segment page size %d", h.PageSize)
	}
	stride := 8 + h.PageSize
	if len(body)%stride != 0 {
		return h, nil, nil, fmt.Errorf("dataplane: segment body %d bytes is not a multiple of %d", len(body), stride)
	}
	n := len(body) / stride
	if uint64(n) != h.ToLSN-h.FromLSN {
		return h, nil, nil, fmt.Errorf("dataplane: segment holds %d frames, header claims %d", n, h.ToLSN-h.FromLSN)
	}
	frames := make([]sqlitewal.Frame, n)
	for i := 0; i < n; i++ {
		off := i * stride
		frames[i] = sqlitewal.Frame{
			Pgno:   binary.BigEndian.Uint32(body[off:]),
			Commit: binary.BigEndian.Uint32(body[off+4:]),
			Data:   body[off+8 : off+stride],
		}
	}
	return h, frames, nil, nil
}
