// Package dataplane implements the fencing protocol — rules E1–E6 from the
// design docs — as code. It is the M3 crown jewel: host-side WAL segments
// stamped with epochs (E2), a gateway that accepts them only under the
// current epoch and seals forward (E3), a stream manifest in object storage
// mutated exclusively by CAS (E4), fenced checkpoint registration (E5), and
// restore as checkpoint + WAL-delta replay (E6). Point-in-time restore is
// branching, never truncation (D4).
//
// FencingProtocol.tla is the arbiter of this package's semantics: the
// deterministic-simulation harness (internal/dataplane/sim) drives these
// exact functions through spec-mirrored schedules and asserts the spec's
// invariants I1–I4. When this code and the spec disagree, the spec wins.
package dataplane

import "fmt"

// Epoch is the fencing token (E1): cell_generation ‖ counter, compared as
// one integer. The cell-generation prefix means a cell-ownership transfer
// fences every app in the cell at once. Zero means "before any epoch".
type Epoch uint64

func NewEpoch(cellGen, counter uint32) Epoch {
	return Epoch(cellGen)<<32 | Epoch(counter)
}

func (e Epoch) CellGen() uint32 { return uint32(e >> 32) }
func (e Epoch) Counter() uint32 { return uint32(e) }

func (e Epoch) String() string {
	return fmt.Sprintf("g%d.c%d", e.CellGen(), e.Counter())
}
