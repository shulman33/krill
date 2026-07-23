// Package sqlitewal reads the SQLite database and WAL file formats without
// SQLite: the host agent tails a guest's -wal file from OUTSIDE the VM
// (protocol doc §1.1 — epoch stamping never lives in untrusted guest code),
// so it needs to parse frames, verify the cumulative checksum chain, detect
// WAL resets, and replay frames onto a database image at an exact commit
// boundary.
//
// Format reference: https://www.sqlite.org/fileformat2.html (§WAL). Two
// properties make untrusted-source tailing sound:
//
//   - every frame carries the WAL header's salts, so frames from a previous
//     WAL generation (before a checkpoint RESTART) are mechanically
//     distinguishable, and
//   - the checksum chain is cumulative, so a torn or in-progress write
//     invalidates itself and everything after it — a reader can never
//     mistake a partial transaction for a complete one.
package sqlitewal

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	// WALHeaderSize and FrameHeaderSize are fixed by the file format.
	WALHeaderSize   = 32
	FrameHeaderSize = 24

	magicLE = 0x377f0682 // checksums computed over little-endian words
	magicBE = 0x377f0683 // checksums computed over big-endian words
)

var (
	// ErrSaltChange: the WAL was reset (checkpoint RESTART/TRUNCATE) since
	// the cursor was taken; already-consumed frames were checkpointed into
	// the main database file. The caller re-bases (ships a fresh checkpoint)
	// and starts a new cursor.
	ErrSaltChange = errors.New("sqlitewal: WAL salts changed since cursor (WAL was reset)")
	// ErrNotWAL: the bytes do not start with a valid WAL header.
	ErrNotWAL = errors.New("sqlitewal: not a valid WAL header")
)

// Frame is one WAL frame: a page image, plus the commit marker.
type Frame struct {
	Pgno uint32
	// Commit is nonzero only on commit frames: the database size in pages
	// after this transaction. A transaction is frames up to and including a
	// commit frame; frames after the last commit are uncommitted and must
	// never ship.
	Commit uint32
	Data   []byte
}

// Header is the parsed 32-byte WAL header.
type Header struct {
	BigEndian     bool // checksum word order (magic 0x377f0683)
	PageSize      int
	CheckpointSeq uint32
	Salt1, Salt2  uint32
	c1, c2        uint32 // header checksum = seed of the frame chain
}

// Cursor is resumable tail state: how many frames of which WAL generation
// have been consumed, and the checksum-chain state right after them. The
// zero Cursor means "never scanned".
type Cursor struct {
	Init         bool   `json:"init"`
	Salt1        uint32 `json:"salt1"`
	Salt2        uint32 `json:"salt2"`
	Frames       int    `json:"frames"` // frames consumed so far
	S0           uint32 `json:"s0"`
	S1           uint32 `json:"s1"`
	BigEndian    bool   `json:"big_endian"`
	PageSize     int    `json:"page_size"`
	CheckpointSq uint32 `json:"ckpt_seq"`
}

// ParseHeader validates the 32-byte WAL header, including its own checksum.
func ParseHeader(b []byte) (Header, error) {
	if len(b) < WALHeaderSize {
		return Header{}, ErrNotWAL
	}
	magic := binary.BigEndian.Uint32(b[0:4])
	if magic != magicLE && magic != magicBE {
		return Header{}, ErrNotWAL
	}
	h := Header{
		BigEndian:     magic == magicBE,
		PageSize:      int(binary.BigEndian.Uint32(b[8:12])),
		CheckpointSeq: binary.BigEndian.Uint32(b[12:16]),
		Salt1:         binary.BigEndian.Uint32(b[16:20]),
		Salt2:         binary.BigEndian.Uint32(b[20:24]),
		c1:            binary.BigEndian.Uint32(b[24:28]),
		c2:            binary.BigEndian.Uint32(b[28:32]),
	}
	if v := binary.BigEndian.Uint32(b[4:8]); v != 3007000 {
		return Header{}, fmt.Errorf("%w: unknown version %d", ErrNotWAL, v)
	}
	if h.PageSize < 512 || h.PageSize > 65536 || h.PageSize&(h.PageSize-1) != 0 {
		return Header{}, fmt.Errorf("%w: impossible page size %d", ErrNotWAL, h.PageSize)
	}
	s0, s1 := checksum(h.BigEndian, 0, 0, b[0:24])
	if s0 != h.c1 || s1 != h.c2 {
		return Header{}, fmt.Errorf("%w: header checksum mismatch", ErrNotWAL)
	}
	return h, nil
}

// ScanResult is what one tailing pass yields.
type ScanResult struct {
	Header Header
	// Frames are the newly valid frames past the cursor, committed or not.
	// Callers shipping durability must cut at LastCommit.
	Frames []Frame
	Cursor Cursor
}

// Scan parses wal (the full -wal file bytes) and returns frames the cursor
// has not yet consumed, verifying salts and the cumulative checksum chain.
// It stops silently at the first invalid frame — that is the durable tail.
// A cursor from a previous WAL generation returns ErrSaltChange.
//
// An empty or headerless wal with an uninitialized cursor returns an empty
// result (a database that has never been written in WAL mode has no WAL).
func Scan(wal []byte, cur Cursor) (ScanResult, error) {
	if len(wal) < WALHeaderSize {
		if !cur.Init {
			return ScanResult{Cursor: cur}, nil
		}
		// The WAL existed (cursor consumed frames from it) and is now too
		// short to even hold a header: it was truncated by a checkpoint.
		return ScanResult{}, ErrSaltChange
	}
	h, err := ParseHeader(wal)
	if err != nil {
		if !cur.Init {
			return ScanResult{Cursor: cur}, nil
		}
		return ScanResult{}, fmt.Errorf("cursor is live but WAL header is gone: %w", err)
	}

	if cur.Init && (cur.Salt1 != h.Salt1 || cur.Salt2 != h.Salt2) {
		return ScanResult{}, ErrSaltChange
	}
	if !cur.Init {
		cur = Cursor{
			Init: true, Salt1: h.Salt1, Salt2: h.Salt2,
			S0: h.c1, S1: h.c2, // frame chain seeds from the header checksum
			BigEndian: h.BigEndian, PageSize: h.PageSize, CheckpointSq: h.CheckpointSeq,
		}
	}

	res := ScanResult{Header: h, Cursor: cur}
	frameSize := FrameHeaderSize + h.PageSize
	s0, s1 := cur.S0, cur.S1
	for i := cur.Frames; ; i++ {
		off := WALHeaderSize + i*frameSize
		if off+frameSize > len(wal) {
			break // no complete frame here
		}
		fh := wal[off : off+FrameHeaderSize]
		if binary.BigEndian.Uint32(fh[8:12]) != h.Salt1 ||
			binary.BigEndian.Uint32(fh[12:16]) != h.Salt2 {
			break // frame from an older generation: past the valid tail
		}
		data := wal[off+FrameHeaderSize : off+frameSize]
		t0, t1 := checksum(h.BigEndian, s0, s1, fh[0:8])
		t0, t1 = checksum(h.BigEndian, t0, t1, data)
		if t0 != binary.BigEndian.Uint32(fh[16:20]) || t1 != binary.BigEndian.Uint32(fh[20:24]) {
			break // torn or in-progress write: chain is broken here
		}
		s0, s1 = t0, t1
		res.Frames = append(res.Frames, Frame{
			Pgno:   binary.BigEndian.Uint32(fh[0:4]),
			Commit: binary.BigEndian.Uint32(fh[4:8]),
			Data:   append([]byte(nil), data...),
		})
		res.Cursor.Frames = i + 1
		res.Cursor.S0, res.Cursor.S1 = s0, s1
	}
	return res, nil
}

// LastCommit returns the index of the last commit frame in frames, or -1.
func LastCommit(frames []Frame) int {
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].Commit != 0 {
			return i
		}
	}
	return -1
}

// checksum is SQLite's WAL checksum: pairs of 32-bit words in the order the
// magic dictates, Fibonacci-style accumulation. Stored values are always
// big-endian; only the WORD INTERPRETATION order varies.
func checksum(bigEndian bool, s0, s1 uint32, b []byte) (uint32, uint32) {
	for i := 0; i+8 <= len(b); i += 8 {
		var x0, x1 uint32
		if bigEndian {
			x0 = binary.BigEndian.Uint32(b[i : i+4])
			x1 = binary.BigEndian.Uint32(b[i+4 : i+8])
		} else {
			x0 = binary.LittleEndian.Uint32(b[i : i+4])
			x1 = binary.LittleEndian.Uint32(b[i+4 : i+8])
		}
		s0 += x0 + s1
		s1 += x1 + s0
	}
	return s0, s1
}

// DBInfo is the slice of the 100-byte database header the data plane needs.
type DBInfo struct {
	PageSize  int
	SizePages uint32
	// ChangeCounter increments on every transaction that modifies the file
	// (in rollback mode) or checkpoint (in WAL mode).
	ChangeCounter uint32
}

// ParseDBHeader reads the main database file header. An EMPTY file is legal
// SQLite (a fresh WAL-mode database keeps everything in the WAL until the
// first checkpoint) and returns PageSize 0.
func ParseDBHeader(b []byte) (DBInfo, error) {
	if len(b) == 0 {
		return DBInfo{}, nil
	}
	if len(b) < 100 || string(b[0:16]) != "SQLite format 3\x00" {
		return DBInfo{}, errors.New("sqlitewal: not a SQLite database header")
	}
	ps := int(binary.BigEndian.Uint16(b[16:18]))
	if ps == 1 {
		ps = 65536
	}
	return DBInfo{
		PageSize:      ps,
		ChangeCounter: binary.BigEndian.Uint32(b[24:28]),
		SizePages:     binary.BigEndian.Uint32(b[28:32]),
	}, nil
}
