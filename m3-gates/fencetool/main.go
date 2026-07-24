// fencetool — the C2 gate's stale-epoch prober. It speaks the data plane's
// own wire format against a LIVE app's stream and attempts exactly what a
// zombie would: a WAL-segment append and a checkpoint registration stamped
// with a superseded epoch. Correct behavior is rejection with the manifest
// left byte-identical.
//
//	fencetool -objstore file:///srv/krill/objstore -app ledger [-stream s0] <cmd>
//
//	dump            print the manifest JSON
//	check           run CheckManifest (I1/I3 off the real manifest)
//	stale-append    attempt an epoch-(current-1) segment append; exit 0 iff fenced
//	stale-register  attempt an epoch-(current-1) checkpoint registration; exit 0 iff fenced
//
// Run it while the app is quiescent (FROZEN): the fsstore CAS serializes
// in-process, and the gate keeps krilld idle during the probe.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/shulman33/krill/internal/dataplane"
	"github.com/shulman33/krill/internal/objstore"
	"github.com/shulman33/krill/internal/sqlitewal"
)

func main() {
	var spec, app, stream string
	flag.StringVar(&spec, "objstore", "", "object store spec (file:///path or gs://bucket)")
	flag.StringVar(&app, "app", "", "app name")
	flag.StringVar(&stream, "stream", "s0", "stream id")
	flag.Parse()
	if spec == "" || app == "" || flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: fencetool -objstore SPEC -app NAME [-stream sN] dump|check|stale-append|stale-register")
		os.Exit(2)
	}
	store, err := objstore.Open(spec)
	if err != nil {
		fatal(err)
	}
	gw := dataplane.New(store)
	ctx := context.Background()
	m, _, err := gw.Load(ctx, app, stream)
	if err != nil {
		fatal(fmt.Errorf("loading manifest: %w", err))
	}

	switch flag.Arg(0) {
	case "dump":
		b, _ := json.MarshalIndent(m, "", "  ")
		fmt.Println(string(b))

	case "check":
		if err := dataplane.CheckManifest(m); err != nil {
			fatal(fmt.Errorf("manifest violates invariants: %w", err))
		}
		fmt.Printf("ok: head=%d cur_epoch=%s segments=%d checkpoints=%d seals=%d\n",
			m.HeadLSN, m.CurEpoch, len(m.Segments), len(m.Checkpoints), len(m.Seals))

	case "stale-append":
		stale, ps := staleParams(m)
		page := make([]byte, ps)
		copy(page, "zombie write, should never land")
		_, err := gw.AppendSegment(ctx, app, stream, stale,
			[]sqlitewal.Frame{{Pgno: 1, Commit: 1, Data: page}}, ps)
		verdict("stale-append", stale, m.CurEpoch, err)

	case "stale-register":
		stale, _ := staleParams(m)
		_, err := gw.RegisterCheckpoint(ctx, app, stream, stale, m.HeadLSN, []byte("forged lineage"))
		verdict("stale-register", stale, m.CurEpoch, err)

	default:
		fatal(fmt.Errorf("unknown command %q", flag.Arg(0)))
	}
}

func staleParams(m *dataplane.Manifest) (dataplane.Epoch, int) {
	if m.CurEpoch == 0 {
		fatal(errors.New("stream has no epoch yet — wake the app once first"))
	}
	ps := m.PageSize
	if ps == 0 {
		ps = 4096
	}
	return m.CurEpoch - 1, ps
}

func verdict(what string, stale, cur dataplane.Epoch, err error) {
	switch {
	case errors.Is(err, dataplane.ErrFenced):
		fmt.Printf("FENCED (correct): %s with epoch %s vs current %s: %v\n", what, stale, cur, err)
	case err == nil:
		fmt.Printf("ACCEPTED (FENCING BROKEN): %s with epoch %s landed against current %s\n", what, stale, cur)
		os.Exit(1)
	default:
		fatal(fmt.Errorf("%s failed for a non-fencing reason: %w", what, err))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fencetool:", err)
	os.Exit(2)
}
