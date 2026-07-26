package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/shulman33/krill/internal/objstore"
	"github.com/shulman33/krill/internal/regbackup"
)

// durability answers one question: if this host died right now, what would
// survive? It live-checks the object store's conditional PUT and lists the
// registry snapshots that have actually landed off-box.
func (c *client) durability(args []string) error {
	fs := c.flags("durability")
	asJSON := fs.Bool("json", false, "raw JSON")
	fs.Parse(args)

	raw, storeErr := c.get("/v1/objstore")
	if *asJSON {
		if storeErr != nil {
			return storeErr
		}
		fmt.Println(string(raw))
		return c.printBackups(true, 0)
	}
	if storeErr != nil {
		// A failing check still returns a body; c.get turns non-2xx into an
		// error, so say what it said.
		fmt.Printf("object store: ✗ %v\n", storeErr)
	} else {
		var st struct {
			Spec       string `json:"spec"`
			Store      string `json:"store"`
			BackupSpec string `json:"backup_spec"`
			OK         bool   `json:"ok"`
			Error      string `json:"error"`
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			return err
		}
		where := st.Store
		if where == "" {
			where = st.Spec
		}
		mark := "✓"
		if !st.OK {
			mark = "✗"
		}
		fmt.Printf("object store: %s %s\n", mark, where)
		fmt.Println("  conditional PUT (E4): verified by a live compare-and-swap round trip")
		if st.BackupSpec != "" && st.BackupSpec != st.Spec {
			fmt.Printf("  registry backups go to: %s\n", st.BackupSpec)
		}
	}
	return c.printBackups(false, 5)
}

func (c *client) printBackups(quiet bool, limit int) error {
	raw, err := c.get("/v1/registry/backups")
	if err != nil {
		fmt.Printf("registry backups: ✗ %v\n", err)
		return nil
	}
	if quiet {
		fmt.Println(string(raw))
		return nil
	}
	var list []regbackup.Info
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("registry backups: NONE — the epoch mint exists only on this host")
		return nil
	}
	age := time.Since(list[0].TakenAt).Round(time.Minute)
	fmt.Printf("registry backups: %d, newest %s old (%d apps, max epoch %d)\n",
		len(list), age, list[0].Apps, list[0].MaxEpoch)
	fmt.Printf("  after restoring one, restart krilld with --cell-gen %d or higher (E1)\n",
		list[0].RestoreCellGen)
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	for _, b := range list {
		fmt.Printf("  %-44s %8s  %s\n", b.Key, humanBytes(b.Bytes),
			b.TakenAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// backup forces a snapshot now — before anything scary, and as the
// verification step when backups are first configured.
func (c *client) backup(args []string) error {
	fs := c.flags("backup")
	asJSON := fs.Bool("json", false, "raw JSON")
	fs.Parse(args)
	c.http.Timeout = 5 * time.Minute
	resp, err := c.http.Post(c.admin+"/v1/registry/backup", "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("backup: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if *asJSON {
		fmt.Println(string(raw))
		return nil
	}
	var info regbackup.Info
	if err := json.Unmarshal(raw, &info); err != nil {
		return err
	}
	fmt.Printf("✓ registry backed up: %s (%s compressed, %s on disk)\n",
		info.Key, humanBytes(info.Bytes), humanBytes(info.DBBytes))
	fmt.Printf("  %d apps, max epoch %d, taken under cell-gen %d\n",
		info.Apps, info.MaxEpoch, info.CellGen)
	fmt.Printf("  sha256 %s\n", info.SHA256)
	return nil
}

// objstoreCopy mirrors one object store into another. It talks to the stores
// directly, not to the admin API, because it is the tool you reach for when
// krilld is stopped: repointing --objstore at an empty bucket does not
// migrate history, it declares every stream empty (E4), and the next wake
// faithfully rebuilds every data disk to match.
func (c *client) objstoreCopy(args []string) error {
	fs := c.flags("objstore-copy")
	from := fs.String("from", "", "source store: file:///path or gs://bucket/prefix")
	to := fs.String("to", "", "destination store")
	prefix := fs.String("prefix", "", "limit to keys under this prefix")
	force := fs.Bool("force", false, "overwrite destination objects whose contents differ")
	dryRun := fs.Bool("dry-run", false, "list what would move, copy nothing")
	quiet := fs.Bool("quiet", false, "no per-object output")
	parseFlexible(fs, args)
	if *from == "" || *to == "" {
		return fmt.Errorf("usage: krill objstore-copy --from <spec> --to <spec> [--prefix p] [--dry-run] [--force]\n" +
			"  specs: file:///abs/path | gs://bucket/prefix")
	}
	if *from == *to {
		return fmt.Errorf("--from and --to are the same store")
	}
	src, err := objstore.Open(*from)
	if err != nil {
		return err
	}
	dst, err := objstore.Open(*to)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if *dryRun {
		keys, err := src.List(ctx, *prefix)
		if err != nil {
			return err
		}
		for _, k := range keys {
			fmt.Println("would copy", k)
		}
		fmt.Printf("%d objects under %q at %s\n", len(keys), *prefix, *from)
		return nil
	}
	start := time.Now()
	rep, err := objstore.Copy(ctx, dst, src, objstore.CopyOpts{
		Prefix: *prefix,
		Force:  *force,
		OnObject: func(key string, size int, action string) {
			if !*quiet {
				fmt.Fprintf(os.Stderr, "  %-10s %-60s %s\n", action, key, humanBytes(int64(size)))
			}
		},
	})
	// Report progress even on failure: a partial copy is resumable and the
	// operator needs to know how far it got.
	fmt.Printf("%d copied (%s), %d already present, in %s\n",
		rep.Copied, humanBytes(rep.Bytes), rep.Skipped, time.Since(start).Round(time.Millisecond))
	if err != nil {
		return err
	}
	fmt.Printf("%s → %s\n", *from, *to)
	return nil
}

func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/(1024*1024*1024))
	}
}
