package dataplane

import "fmt"

// CheckManifest verifies the structural invariants a correct gateway can
// never break, read directly off a real manifest. The simulation harness
// runs it after every step; the C2 gate tool runs it against a live app's
// stream. The names map to the spec:
//
//	I1  EpochsMonotoneInWAL  — segment epochs are non-decreasing
//	I3  SnapshotsOnHistory   — every checkpoint's claimed prefix was
//	                           written entirely by epochs <= its own
//	E4 sanity                — CurEpoch bounds every shipped epoch
func CheckManifest(m *Manifest) error {
	base := uint64(0)
	if m.Parent != nil {
		base = m.Parent.LSN
	}

	// Segment chain: contiguous, non-empty, ending at HeadLSN.
	prevTo := base
	var prevEpoch Epoch
	var maxEpoch Epoch
	for i, s := range m.Segments {
		if s.FromLSN != prevTo {
			return fmt.Errorf("segment %d starts at %d, previous ended at %d (gap or overlap)", i, s.FromLSN, prevTo)
		}
		if s.ToLSN <= s.FromLSN {
			return fmt.Errorf("segment %d is empty or inverted (%d, %d]", i, s.FromLSN, s.ToLSN)
		}
		// I1: single writer — once an epoch is superseded in the stream, no
		// lower epoch ever appends again.
		if s.Epoch < prevEpoch {
			return fmt.Errorf("I1 violated: segment %d epoch %s after epoch %s", i, s.Epoch, prevEpoch)
		}
		prevEpoch = s.Epoch
		if s.Epoch > maxEpoch {
			maxEpoch = s.Epoch
		}
		prevTo = s.ToLSN
	}
	if prevTo != m.HeadLSN {
		return fmt.Errorf("segments end at %d but head is %d", prevTo, m.HeadLSN)
	}

	// E4 sanity: the sealed epoch never trails what was shipped.
	if maxEpoch > m.CurEpoch {
		return fmt.Errorf("CurEpoch %s < shipped epoch %s", m.CurEpoch, maxEpoch)
	}

	// Seals are strictly increasing in epoch.
	var prevSeal Epoch
	for i, s := range m.Seals {
		if s.Epoch <= prevSeal {
			return fmt.Errorf("seal %d epoch %s not above predecessor %s", i, s.Epoch, prevSeal)
		}
		if s.AtLSN > m.HeadLSN {
			return fmt.Errorf("seal %d at LSN %d beyond head %d", i, s.AtLSN, m.HeadLSN)
		}
		prevSeal = s.Epoch
	}

	// I3: snapshot lineage. A checkpoint at (e, L) claims the prefix up to
	// L was written by epochs <= e; any overlapping segment with a higher
	// epoch means the manifest lies about ancestry (the slow waker's forgery).
	for i, c := range m.Checkpoints {
		if c.LSN > m.HeadLSN {
			return fmt.Errorf("I3 violated: checkpoint %d at LSN %d beyond head %d", i, c.LSN, m.HeadLSN)
		}
		for _, s := range m.Segments {
			if s.FromLSN < c.LSN && s.Epoch > c.Epoch {
				return fmt.Errorf("I3 violated: checkpoint %d (epoch %s, lsn %d) claims a prefix containing epoch-%s segment (%d,%d]",
					i, c.Epoch, c.LSN, s.Epoch, s.FromLSN, s.ToLSN)
			}
		}
	}
	return nil
}
