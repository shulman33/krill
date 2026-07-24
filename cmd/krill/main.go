// krill — the Krill CLI. A thin client of krilld's admin API: the deploy
// path in one command, plus the day-to-day verbs.
//
//	krill deploy [dir]   directory → running app → URL printed
//	krill apps           list apps and their states
//	krill status <app>
//	krill logs <app>     serial tail + structured runtime errors
//	krill wake <app>
//	krill freeze <app>
//	krill delete <app>
//	krill stream <app>   data-plane stream: head LSN, epoch, segments, branches
//	krill restore <app> --at-lsn N | --at-time RFC3339   PITR (branching)
//
// The admin API address comes from --admin or KRILL_ADMIN (default
// http://127.0.0.1:9091 — run on the host, or bring an SSH tunnel).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/shulman33/krill/internal/builder"
	"github.com/shulman33/krill/internal/guestlog"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "krill:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: krill <deploy|apps|status|logs|wake|freeze|delete|stream|restore> [args]\n(see the package comment in cmd/krill)")
	}
	c := &client{admin: envOr("KRILL_ADMIN", "http://127.0.0.1:9091")}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "deploy":
		return c.deploy(rest)
	case "apps":
		return c.apps(rest)
	case "status":
		return c.appJSON(rest, "status", "")
	case "wake":
		return c.appPost(rest, "wake")
	case "freeze":
		return c.appPost(rest, "freeze")
	case "delete":
		return c.delete(rest)
	case "logs":
		return c.logs(rest)
	case "stream":
		return c.appJSON(rest, "stream", "/stream")
	case "restore":
		return c.restore(rest)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// restore is PITR: branch the app's data stream at a past point and rebuild
// its data disk there. The old stream is never modified — `krill stream`
// shows every branch, and restoring back is always possible.
func (c *client) restore(args []string) error {
	fs := c.flags("restore")
	atLSN := fs.Uint64("at-lsn", 0, "stream LSN to restore to (see `krill stream`)")
	atTime := fs.String("at-time", "", "RFC3339 instant to restore to (host clock)")
	fromStream := fs.String("from-stream", "", "branch source stream (default: current; name an ancestor to restore back past an earlier restore)")
	pos := parseFlexible(fs, args)
	if len(pos) != 1 || (*atLSN == 0 && *atTime == "") {
		return fmt.Errorf("usage: krill restore <app> --at-lsn N | --at-time RFC3339 [--from-stream sN]")
	}
	body, _ := json.Marshal(map[string]any{"at_lsn": *atLSN, "at_time": *atTime, "stream": *fromStream})
	c.http.Timeout = 3 * time.Minute
	resp, err := c.http.Post(c.admin+"/v1/apps/"+pos[0]+"/restore", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("restore: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	fmt.Println(string(raw))
	return nil
}

type client struct {
	admin string
	http  http.Client
}

func (c *client) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&c.admin, "admin", c.admin, "krilld admin API base URL (env KRILL_ADMIN)")
	return fs
}

// parseFlexible parses flags while collecting positional arguments wherever
// they appear. Go's flag package stops at the first positional, which turns
// "krill deploy dir --json" into silently ignored flags — exactly the kind
// of footgun an agent trips on.
func parseFlexible(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args)
		rem := fs.Args()
		if len(rem) == 0 {
			return pos
		}
		pos = append(pos, rem[0])
		args = rem[1:]
	}
}

// deployResp mirrors the admin API's deploy payload (the fields we render).
type deployResp struct {
	App struct {
		Name       string `json:"name"`
		State      string `json:"state"`
		SubnetIdx  int    `json:"subnet_idx"`
		GuestPort  int    `json:"guest_port"`
		VCPUs      int    `json:"vcpus"`
		MemMiB     int    `json:"mem_mib"`
		LastWakeMs int64  `json:"last_wake_ms"`
	} `json:"app"`
	URL       string           `json:"url"`
	Created   bool             `json:"created"`
	Ready     bool             `json:"ready"`
	WakeError string           `json:"wake_error"`
	Errors    []guestlog.Error `json:"errors"`
	SizeMB    int              `json:"size_mb"`
	Warnings  []string         `json:"warnings"`
	BuildSecs float64          `json:"build_secs"`
	CurlHint  string           `json:"curl_hint"`
}

func (c *client) deploy(args []string) error {
	fs := c.flags("deploy")
	name := fs.String("name", "", "app name (default: directory basename)")
	port := fs.Int("port", 0, "guest port (default: EXPOSE from the Dockerfile)")
	vcpus := fs.Int("vcpus", 0, "vCPUs (default: 1, or unchanged on redeploy)")
	mem := fs.Int("mem", 0, "MiB of RAM (default: 512, or unchanged on redeploy)")
	sizeMB := fs.Int("size-mb", 0, "rootfs size in MB (default: auto)")
	noVerify := fs.Bool("no-verify", false, "skip the post-deploy verification wake")
	asJSON := fs.Bool("json", false, "print the raw API response")
	pos := parseFlexible(fs, args)

	dir := "."
	if len(pos) > 0 {
		dir = pos[0]
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a directory", abs)
	}
	if _, err := os.Stat(filepath.Join(abs, "Dockerfile")); err != nil {
		return fmt.Errorf("%s has no Dockerfile — krill deploys docker build contexts", abs)
	}
	appName := *name
	if appName == "" {
		appName = sanitizeName(filepath.Base(abs))
	}

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(builder.PackTarGz(abs, pw)) }()

	q := url.Values{}
	setIf := func(k string, v int) {
		if v > 0 {
			q.Set(k, fmt.Sprint(v))
		}
	}
	setIf("guest_port", *port)
	setIf("vcpus", *vcpus)
	setIf("mem_mib", *mem)
	setIf("size_mb", *sizeMB)
	if *noVerify {
		q.Set("verify", "false")
	}
	u := fmt.Sprintf("%s/v1/apps/%s/deploy?%s", c.admin, appName, q.Encode())

	fmt.Fprintf(os.Stderr, "building and deploying %s from %s ...\n", appName, abs)
	c.http.Timeout = 15 * time.Minute
	resp, err := c.http.Post(u, "application/gzip", pr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if *asJSON {
		fmt.Println(string(raw))
	}
	if resp.StatusCode == http.StatusUnprocessableEntity {
		var body map[string]string
		json.Unmarshal(raw, &body)
		if !*asJSON {
			fmt.Fprintf(os.Stderr, "✗ build failed at %s:\n%s\n", body["stage"], body["build_log"])
		}
		return fmt.Errorf("build failed")
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("deploy: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var dr deployResp
	if err := json.Unmarshal(raw, &dr); err != nil {
		return fmt.Errorf("bad response: %w", err)
	}
	if *asJSON {
		if !dr.Ready && !*noVerify {
			return fmt.Errorf("app deployed but not ready")
		}
		return nil
	}

	verb := "updated"
	if dr.Created {
		verb = "created"
	}
	fmt.Printf("✓ deployed %s (%s, %d MB image, build %.1fs)\n", dr.App.Name, verb, dr.SizeMB, dr.BuildSecs)
	fmt.Printf("  url:  %s\n", dr.URL)
	fmt.Printf("  curl: %s\n", dr.CurlHint)
	for _, w := range dr.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
	if *noVerify {
		return nil
	}
	if dr.Ready {
		fmt.Printf("  ready: yes (first wake %d ms, state %s)\n", dr.App.LastWakeMs, dr.App.State)
		return nil
	}
	fmt.Printf("  ready: NO — %s\n", dr.WakeError)
	printGuestErrors(dr.Errors)
	fmt.Printf("  (full log: krill logs %s)\n", dr.App.Name)
	return fmt.Errorf("app deployed but not ready")
}

func (c *client) apps(args []string) error {
	fs := c.flags("apps")
	asJSON := fs.Bool("json", false, "raw JSON")
	fs.Parse(args)
	raw, err := c.get("/v1/apps")
	if err != nil {
		return err
	}
	if *asJSON {
		fmt.Println(string(raw))
		return nil
	}
	var apps []struct {
		Name          string `json:"name"`
		State         string `json:"state"`
		SnapshotValid bool   `json:"snapshot_valid"`
		VCPUs         int    `json:"vcpus"`
		MemMiB        int    `json:"mem_mib"`
		LastWakeMs    int64  `json:"last_wake_ms"`
	}
	if err := json.Unmarshal(raw, &apps); err != nil {
		return err
	}
	if len(apps) == 0 {
		fmt.Println("no apps registered")
		return nil
	}
	fmt.Printf("%-24s %-13s %-9s %-6s %s\n", "NAME", "STATE", "SNAPSHOT", "RAM", "LAST WAKE")
	for _, a := range apps {
		snap := "-"
		if a.SnapshotValid {
			snap = "valid"
		}
		wake := "-"
		if a.LastWakeMs > 0 {
			wake = fmt.Sprintf("%d ms", a.LastWakeMs)
		}
		fmt.Printf("%-24s %-13s %-9s %-6d %s\n", a.Name, a.State, snap, a.MemMiB, wake)
	}
	return nil
}

func (c *client) logs(args []string) error {
	fs := c.flags("logs")
	tail := fs.Int("tail", 100, "lines of serial log")
	asJSON := fs.Bool("json", false, "raw JSON")
	pos := parseFlexible(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("usage: krill logs <app>")
	}
	raw, err := c.get(fmt.Sprintf("/v1/apps/%s/logs?tail=%d", pos[0], *tail))
	if err != nil {
		return err
	}
	if *asJSON {
		fmt.Println(string(raw))
		return nil
	}
	var body struct {
		Lines  []string         `json:"lines"`
		Errors []guestlog.Error `json:"errors"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	for _, l := range body.Lines {
		fmt.Println(l)
	}
	if len(body.Errors) > 0 {
		fmt.Println("\n--- structured errors ---")
		printGuestErrors(body.Errors)
	}
	return nil
}

func (c *client) appJSON(args []string, cmd, suffix string) error {
	fs := c.flags(cmd)
	pos := parseFlexible(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("usage: krill %s <app>", cmd)
	}
	raw, err := c.get("/v1/apps/" + pos[0] + suffix)
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	return nil
}

func (c *client) appPost(args []string, action string) error {
	fs := c.flags(action)
	pos := parseFlexible(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("usage: krill %s <app>", action)
	}
	c.http.Timeout = 3 * time.Minute
	resp, err := c.http.Post(c.admin+"/v1/apps/"+pos[0]+"/"+action, "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s: %s", action, resp.Status, strings.TrimSpace(string(raw)))
	}
	fmt.Println(string(raw))
	return nil
}

func (c *client) delete(args []string) error {
	fs := c.flags("delete")
	pos := parseFlexible(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("usage: krill delete <app>")
	}
	req, _ := http.NewRequest(http.MethodDelete, c.admin+"/v1/apps/"+pos[0], nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	fmt.Println("deleted", fs.Arg(0))
	return nil
}

func (c *client) get(path string) ([]byte, error) {
	c.http.Timeout = time.Minute
	resp, err := c.http.Get(c.admin + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func printGuestErrors(errs []guestlog.Error) {
	for _, e := range errs {
		fmt.Printf("  [%s] %s\n", e.Kind, e.Message)
		if e.Detail != "" && e.Detail != e.Message {
			for _, l := range strings.Split(e.Detail, "\n") {
				fmt.Printf("    %s\n", l)
			}
		}
		if e.Hint != "" {
			fmt.Printf("    hint: %s\n", e.Hint)
		}
	}
}

var nameCleanRE = regexp.MustCompile(`[^a-z0-9-]+`)

// sanitizeName turns a directory basename into a valid app name (DNS label).
func sanitizeName(base string) string {
	n := strings.ToLower(base)
	n = nameCleanRE.ReplaceAllString(n, "-")
	n = strings.Trim(n, "-")
	if len(n) > 31 {
		n = n[:31]
	}
	if n == "" {
		n = "app"
	}
	return n
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
