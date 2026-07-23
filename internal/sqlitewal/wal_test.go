package sqlitewal

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// openWAL opens a WAL-mode database with autocheckpoint OFF, so frames stay
// in the -wal file where the tailer can see them (in production the tailer
// races the guest's checkpoints and re-bases on ErrSaltChange; tests pin the
// WAL down to make assertions exact).
func openWAL(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite",
		path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=wal_autocheckpoint(0)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func insertRows(t *testing.T, db *sql.DB, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		mustExec(t, db, "INSERT INTO kv (k, v) VALUES (?, ?)", i, fmt.Sprintf("value-%d", i))
	}
}

func queryState(t *testing.T, path string) (count int, sum int) {
	t.Helper()
	db := openWAL(t, path)
	if err := db.QueryRow("SELECT COUNT(*), COALESCE(SUM(k), 0) FROM kv").Scan(&count, &sum); err != nil {
		t.Fatal(err)
	}
	return count, sum
}

func TestScanAndReplayRealWAL(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")
	db := openWAL(t, dbPath)

	mustExec(t, db, "CREATE TABLE kv (k INTEGER PRIMARY KEY, v TEXT)")
	// Flush the schema into the main file and reset the WAL, then capture
	// the base image — the "checkpoint blob" of this test.
	mustExec(t, db, "PRAGMA wal_checkpoint(TRUNCATE)")
	base, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := ParseDBHeader(base)
	if err != nil {
		t.Fatal(err)
	}
	if info.PageSize == 0 {
		t.Fatal("base image has no page size")
	}

	// Phase 1: 50 rows in 50 transactions.
	insertRows(t, db, 0, 50)
	wal, err := os.ReadFile(dbPath + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Scan(wal, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Frames) == 0 {
		t.Fatal("no frames scanned from a WAL with 50 committed transactions")
	}
	if lc := LastCommit(res.Frames); lc != len(res.Frames)-1 {
		t.Fatalf("expected the last frame to be a commit frame; LastCommit=%d of %d", lc, len(res.Frames))
	}
	if res.Header.PageSize != info.PageSize {
		t.Fatalf("WAL page size %d != db page size %d", res.Header.PageSize, info.PageSize)
	}

	// Replay onto the base image must equal the live database.
	img, err := Replay(base, info.PageSize, res.Frames)
	if err != nil {
		t.Fatal(err)
	}
	replayed := filepath.Join(dir, "replayed.db")
	if err := os.WriteFile(replayed, img, 0o644); err != nil {
		t.Fatal(err)
	}
	if c, s := queryState(t, replayed); c != 50 || s != 49*50/2 {
		t.Fatalf("replayed phase 1: count=%d sum=%d, want 50/%d", c, s, 49*50/2)
	}

	// Phase 2: cursor resume sees only the delta; incremental replay
	// (segment-wise, the way the restore path consumes segments) matches.
	cur := res.Cursor
	insertRows(t, db, 100, 10)
	wal2, err := os.ReadFile(dbPath + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	res2, err := Scan(wal2, cur)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Frames) == 0 || len(res2.Frames) >= len(res.Frames)+10 {
		t.Fatalf("delta scan returned %d frames (phase 1 had %d)", len(res2.Frames), len(res.Frames))
	}
	img2, err := Replay(img, info.PageSize, res2.Frames)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replayed, img2, 0o644); err != nil {
		t.Fatal(err)
	}
	wantSum := 49*50/2 + (100+109)*10/2
	if c, s := queryState(t, replayed); c != 60 || s != wantSum {
		t.Fatalf("incremental replay: count=%d sum=%d, want 60/%d", c, s, wantSum)
	}

	// A full rescan from zero plus one-shot replay agrees too.
	resAll, err := Scan(wal2, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	imgAll, err := Replay(base, info.PageSize, resAll.Frames)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replayed, imgAll, 0o644); err != nil {
		t.Fatal(err)
	}
	if c, s := queryState(t, replayed); c != 60 || s != wantSum {
		t.Fatalf("full replay: count=%d sum=%d, want 60/%d", c, s, wantSum)
	}

	// WAL reset: a checkpoint RESTART plus one more write rewinds the WAL
	// with fresh salts. The stale cursor must be told, not fed garbage.
	mustExec(t, db, "PRAGMA wal_checkpoint(RESTART)")
	insertRows(t, db, 200, 1)
	wal3, err := os.ReadFile(dbPath + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(wal3, res2.Cursor); !errors.Is(err, ErrSaltChange) {
		t.Fatalf("scan across WAL reset: %v, want ErrSaltChange", err)
	}
	// A fresh cursor picks up the new generation cleanly.
	resNew, err := Scan(wal3, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resNew.Frames) == 0 {
		t.Fatal("no frames after WAL reset + insert")
	}
}

func TestScanStopsAtTornWrites(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "app.db")
	db := openWAL(t, dbPath)
	mustExec(t, db, "CREATE TABLE kv (k INTEGER PRIMARY KEY, v TEXT)")
	mustExec(t, db, "PRAGMA wal_checkpoint(TRUNCATE)")
	insertRows(t, db, 0, 10)

	wal, err := os.ReadFile(dbPath + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	full, err := Scan(wal, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	n := len(full.Frames)
	if n < 2 {
		t.Fatalf("need >=2 frames, got %d", n)
	}

	// Truncate into the middle of the last frame: it disappears, silently.
	torn, err := Scan(wal[:len(wal)-10], Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(torn.Frames) != n-1 {
		t.Fatalf("torn scan: %d frames, want %d", len(torn.Frames), n-1)
	}

	// Flip a byte in frame 2's page data: the chain breaks there and
	// everything after is untrusted, even though it checksums fine locally.
	corrupt := append([]byte(nil), wal...)
	frameSize := FrameHeaderSize + full.Header.PageSize
	corrupt[WALHeaderSize+frameSize+FrameHeaderSize+5] ^= 0xff
	broken, err := Scan(corrupt, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(broken.Frames) != 1 {
		t.Fatalf("corrupt scan: %d frames, want 1 (chain must break at the corruption)", len(broken.Frames))
	}
}

// buildWAL fabricates a syntactically perfect WAL so the big-endian-checksum
// code path gets exercised (stock SQLite on arm64/x86 only ever writes the
// little-endian flavor, which the real-file tests above anchor).
func buildWAL(bigEnd bool, pageSize int, salt1, salt2 uint32, frames []Frame) []byte {
	magic := uint32(magicLE)
	if bigEnd {
		magic = magicBE
	}
	hdr := make([]byte, WALHeaderSize)
	binary.BigEndian.PutUint32(hdr[0:4], magic)
	binary.BigEndian.PutUint32(hdr[4:8], 3007000)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(pageSize))
	binary.BigEndian.PutUint32(hdr[12:16], 1) // checkpoint seq
	binary.BigEndian.PutUint32(hdr[16:20], salt1)
	binary.BigEndian.PutUint32(hdr[20:24], salt2)
	s0, s1 := checksum(bigEnd, 0, 0, hdr[0:24])
	binary.BigEndian.PutUint32(hdr[24:28], s0)
	binary.BigEndian.PutUint32(hdr[28:32], s1)

	out := hdr
	for _, f := range frames {
		fh := make([]byte, FrameHeaderSize)
		binary.BigEndian.PutUint32(fh[0:4], f.Pgno)
		binary.BigEndian.PutUint32(fh[4:8], f.Commit)
		binary.BigEndian.PutUint32(fh[8:12], salt1)
		binary.BigEndian.PutUint32(fh[12:16], salt2)
		s0, s1 = checksum(bigEnd, s0, s1, fh[0:8])
		s0, s1 = checksum(bigEnd, s0, s1, f.Data)
		binary.BigEndian.PutUint32(fh[16:20], s0)
		binary.BigEndian.PutUint32(fh[20:24], s1)
		out = append(out, fh...)
		out = append(out, f.Data...)
	}
	return out
}

func TestSyntheticByteOrders(t *testing.T) {
	for _, bigEnd := range []bool{false, true} {
		t.Run(fmt.Sprintf("bigEndian=%v", bigEnd), func(t *testing.T) {
			const ps = 512
			page1 := make([]byte, ps)
			page2 := make([]byte, ps)
			for i := range page1 {
				page1[i] = byte(i)
				page2[i] = byte(i * 7)
			}
			wal := buildWAL(bigEnd, ps, 0xAABBCCDD, 0x11223344, []Frame{
				{Pgno: 1, Data: page1},
				{Pgno: 2, Commit: 2, Data: page2},
			})
			res, err := Scan(wal, Cursor{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Frames) != 2 {
				t.Fatalf("scanned %d frames, want 2", len(res.Frames))
			}
			if res.Header.BigEndian != bigEnd {
				t.Fatalf("header byte order = %v", res.Header.BigEndian)
			}
			if res.Frames[1].Commit != 2 {
				t.Fatal("lost the commit marker")
			}
			img, err := Replay(nil, ps, res.Frames)
			if err != nil {
				t.Fatal(err)
			}
			if len(img) != 2*ps {
				t.Fatalf("replayed image is %d bytes, want %d", len(img), 2*ps)
			}
			if img[0] != 0 || img[ps+7] != byte(7*7) {
				t.Fatal("replayed pages landed in the wrong slots")
			}
		})
	}
}

func TestUncommittedTailNeverReplays(t *testing.T) {
	const ps = 512
	page := make([]byte, ps)
	frames := []Frame{
		{Pgno: 1, Commit: 1, Data: page},
		{Pgno: 2, Data: page}, // open transaction, no commit
	}
	img, err := Replay(nil, ps, frames)
	if err != nil {
		t.Fatal(err)
	}
	if len(img) != ps {
		t.Fatalf("uncommitted frame leaked into the image: %d bytes, want %d", len(img), ps)
	}
}

func TestParseDBHeaderEmpty(t *testing.T) {
	info, err := ParseDBHeader(nil)
	if err != nil || info.PageSize != 0 {
		t.Fatalf("empty db: %+v err %v", info, err)
	}
	if _, err := ParseDBHeader(make([]byte, 200)); err == nil {
		t.Fatal("garbage accepted as a database header")
	}
}
