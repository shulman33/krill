package sqlitewal

import "fmt"

// Replay applies WAL frames to a database image and returns the image as of
// the LAST COMMIT BOUNDARY in frames — exactly what SQLite's own checkpoint
// would produce. Frames after the last commit are uncommitted and are
// discarded. This is E6's replay half and the checkpoint materializer: a
// restore is checkpoint image + Replay(delta frames).
//
// base may be empty (a fresh WAL-mode database checkpoints nothing until
// its first checkpoint — the whole database lives in frames).
func Replay(base []byte, pageSize int, frames []Frame) ([]byte, error) {
	if pageSize < 512 || pageSize&(pageSize-1) != 0 {
		return nil, fmt.Errorf("sqlitewal: replay with impossible page size %d", pageSize)
	}
	last := LastCommit(frames)
	if last < 0 {
		// No committed transaction in the delta: the image is base as-is.
		return base, nil
	}
	finalPages := int(frames[last].Commit)
	out := make([]byte, finalPages*pageSize)
	copy(out, base) // shrink or grow relative to base; frames overwrite below
	for i := 0; i <= last; i++ {
		f := frames[i]
		if len(f.Data) != pageSize {
			return nil, fmt.Errorf("sqlitewal: frame %d has %d-byte page, want %d", i, len(f.Data), pageSize)
		}
		if f.Pgno == 0 {
			return nil, fmt.Errorf("sqlitewal: frame %d has page number 0", i)
		}
		end := int(f.Pgno) * pageSize
		if end > len(out) {
			// A page beyond the final committed size: a later transaction
			// shrank the file (vacuum). The page is dead; skip it.
			continue
		}
		copy(out[end-pageSize:end], f.Data)
	}
	return out, nil
}
