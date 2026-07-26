package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shulman33/krill/internal/doorman"
)

// The sharing verbs talk to krill-doorman, not krilld — a second endpoint on
// admin port + 1 (9091 → 9092).
//
// They live in the same binary on purpose (ROADMAP decision #10b). The
// krilld/doorman split is an implementation seam, and the CLI is where such
// seams get hidden; a second tool to share the app you just deployed would
// contradict the product's one-line pitch. `objstore-copy` already set the
// precedent of a verb that is not a pure admin-API client.

// doormanAddr resolves the doorman's control API: --doorman, then
// KRILL_DOORMAN, then the admin address with its port incremented.
func doormanAddr(admin, override string) string {
	if override != "" {
		return override
	}
	if v := os.Getenv("KRILL_DOORMAN"); v != "" {
		return v
	}
	scheme, rest, found := strings.Cut(admin, "://")
	if !found {
		scheme, rest = "http", admin
	}
	host, port, err := net.SplitHostPort(rest)
	if err != nil {
		return scheme + "://" + rest
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return scheme + "://" + rest
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(n+1))
}

// share mints a capability link. The link is printed exactly once — the
// doorman stores only its hash — so this output is the artifact.
func (c *client) share(args []string) error {
	fs := c.flags("share")
	override := fs.String("doorman", "", "krill-doorman control API (env KRILL_DOORMAN)")
	plane := fs.String("plane", "use", "what the link grants: use, data or edit")
	label := fs.String("label", "", "a note to yourself about who this link is for")
	expires := fs.String("expires", "", "expire the link after this long (e.g. 72h); default never")
	maxClaims := fs.Int("max-claims", 0, "stop accepting new people after this many claim it (0 = no limit)")
	asJSON := fs.Bool("json", false, "raw JSON")
	pos := parseFlexible(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("usage: krill share <app> [--plane use|data|edit] [--label ...] [--expires 72h] [--max-claims N]")
	}
	who, _ := os.Hostname()
	if u := os.Getenv("USER"); u != "" {
		who = u + "@" + who
	}
	body, _ := json.Marshal(map[string]any{
		"app": pos[0], "plane": *plane, "label": *label,
		"expires_in": *expires, "max_claims": *maxClaims, "created_by": who,
	})
	raw, err := c.doormanPost(doormanAddr(c.admin, *override), "/v1/shares", body)
	if err != nil {
		return err
	}
	if *asJSON {
		fmt.Println(string(raw))
		return nil
	}
	var out struct {
		Share doorman.Share `json:"share"`
		Link  string        `json:"link"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	fmt.Printf("✓ %s can now be shared (%s plane)\n", out.Share.App, out.Share.Plane)
	fmt.Printf("\n  %s\n\n", out.Link)
	fmt.Printf("  Send that link to whoever should have access. They sign in with Google;\n")
	fmt.Printf("  you never need their address in advance.\n")
	if !out.Share.ExpiresAt.IsZero() {
		fmt.Printf("  expires: %s\n", out.Share.ExpiresAt.Format(time.RFC3339))
	}
	fmt.Printf("  revoke:  krill unshare %s --share %s\n", out.Share.App, out.Share.ID)
	fmt.Printf("\n  This link is shown once — the doorman stores only its hash.\n")
	return nil
}

// shares prints the ACL: who has access to what, how they got it, and what
// has been taken away. F1 ends with "Sam can show them it was them"; this is
// that screen.
func (c *client) shares(args []string) error {
	fs := c.flags("shares")
	override := fs.String("doorman", "", "krill-doorman control API (env KRILL_DOORMAN)")
	asJSON := fs.Bool("json", false, "raw JSON")
	all := fs.Bool("all", false, "include revoked links and grants")
	pos := parseFlexible(fs, args)
	app := ""
	if len(pos) > 0 {
		app = pos[0]
	}
	base := doormanAddr(c.admin, *override)
	q := ""
	if app != "" {
		q = "?app=" + app
	}
	sharesRaw, err := c.doormanGet(base, "/v1/shares"+q)
	if err != nil {
		return err
	}
	grantsRaw, err := c.doormanGet(base, "/v1/grants"+q)
	if err != nil {
		return err
	}
	if *asJSON {
		fmt.Printf("{\"shares\":%s,\"grants\":%s}\n",
			strings.TrimSpace(string(sharesRaw)), strings.TrimSpace(string(grantsRaw)))
		return nil
	}
	var shares []doorman.Share
	var grants []doorman.Grant
	if err := json.Unmarshal(sharesRaw, &shares); err != nil {
		return err
	}
	if err := json.Unmarshal(grantsRaw, &grants); err != nil {
		return err
	}

	fmt.Printf("%-12s %-16s %-6s %-8s %-7s %s\n", "LINK", "APP", "PLANE", "CLAIMED", "STATE", "LABEL")
	shown := 0
	for _, s := range shares {
		if s.Revoked && !*all {
			continue
		}
		state := "live"
		if s.Revoked {
			state = "REVOKED"
		}
		claims := strconv.Itoa(s.Claims)
		if s.MaxClaims > 0 {
			claims += "/" + strconv.Itoa(s.MaxClaims)
		}
		fmt.Printf("%-12s %-16s %-6s %-8s %-7s %s\n", s.ID, s.App, s.Plane, claims, state, s.Label)
		shown++
	}
	if shown == 0 {
		fmt.Println("(no live share links)")
	}

	fmt.Printf("\n%-28s %-16s %-6s %-12s %s\n", "WHO", "APP", "PLANE", "VIA", "STATE")
	shown = 0
	for _, g := range grants {
		if g.Revoked && !*all {
			continue
		}
		state := "live"
		if g.Revoked {
			state = "REVOKED " + g.RevokedAt.Format("2006-01-02")
		}
		via := g.ShareID
		if via == "" {
			via = "(direct)"
		}
		fmt.Printf("%-28s %-16s %-6s %-12s %s\n", g.Email, g.App, g.Plane, via, state)
		shown++
	}
	if shown == 0 {
		fmt.Println("(nobody has claimed a link yet)")
	}
	if !*all {
		fmt.Println("\n(--all also shows revoked links and grants)")
	}
	return nil
}

// unshare is F2's operator half. It returns only after the revocation is
// durable at the object store: a success here is a promise no restore can
// take back, and a failure means nothing was revoked at all.
func (c *client) unshare(args []string) error {
	fs := c.flags("unshare")
	override := fs.String("doorman", "", "krill-doorman control API (env KRILL_DOORMAN)")
	user := fs.String("user", "", "revoke this person's access to the app")
	shareID := fs.String("share", "", "revoke this link (and everyone who claimed it)")
	everything := fs.Bool("all", false, "revoke every link and grant on the app")
	reason := fs.String("reason", "", "recorded in the revocation log")
	pos := parseFlexible(fs, args)

	req := map[string]any{"reason": *reason}
	if u := os.Getenv("USER"); u != "" {
		req["by"] = u
	}
	switch {
	case *shareID != "":
		req["kind"], req["share"] = "share", *shareID
	case *everything:
		if len(pos) != 1 {
			return fmt.Errorf("usage: krill unshare <app> --all")
		}
		req["kind"], req["app"] = "app", pos[0]
	case *user != "":
		if len(pos) != 1 {
			return fmt.Errorf("usage: krill unshare <app> --user <email>")
		}
		req["kind"], req["app"], req["email"] = "identity", pos[0], *user
	default:
		return fmt.Errorf("usage: krill unshare <app> --user <email> | --share <id> | <app> --all")
	}
	body, _ := json.Marshal(req)
	raw, err := c.doormanPost(doormanAddr(c.admin, *override), "/v1/revoke", body)
	if err != nil {
		return fmt.Errorf("NOTHING WAS REVOKED: %w", err)
	}
	var rv doorman.Revocation
	if err := json.Unmarshal(raw, &rv); err != nil {
		return err
	}
	fmt.Printf("✓ revoked (%s", rv.Kind)
	if rv.App != "" {
		fmt.Printf(" on %s", rv.App)
	}
	fmt.Printf("), %d grant(s) cut off\n", len(rv.Grants))
	fmt.Printf("  It takes effect on the next request — no restart, no waiting out a session.\n")
	fmt.Printf("  Durable at the object store as %s, so a restore cannot undo it.\n", rv.ID)
	return nil
}

// doormanStatus answers the question worth asking before sharing anything
// with a person: can this box take the access away again?
func (c *client) doormanStatus(args []string) error {
	fs := c.flags("doorman")
	override := fs.String("doorman", "", "krill-doorman control API (env KRILL_DOORMAN)")
	asJSON := fs.Bool("json", false, "raw JSON")
	parseFlexible(fs, args)
	base := doormanAddr(c.admin, *override)
	raw, err := c.doormanGet(base, "/v1/status")
	if err != nil {
		return err
	}
	if *asJSON {
		fmt.Println(string(raw))
		return nil
	}
	var st struct {
		BaseHost      string   `json:"base_host"`
		AuthHost      string   `json:"auth_host"`
		Scheme        string   `json:"scheme"`
		Owners        []string `json:"owners"`
		IdentityKey   string   `json:"identity_key"`
		IdentityPub   string   `json:"identity_pub"`
		Shares        int      `json:"shares"`
		Grants        int      `json:"grants"`
		Revocations   int      `json:"revocations"`
		RevokeDurable bool     `json:"revoke_durable"`
		Objstore      string   `json:"objstore"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	fmt.Printf("doorman     %s (apps at *.%s)\n", base, st.BaseHost)
	fmt.Printf("auth host   %s://%s\n", st.Scheme, st.AuthHost)
	fmt.Printf("owners      %s\n", strings.Join(st.Owners, ", "))
	fmt.Printf("identity    key %s, public %s\n", st.IdentityKey, st.IdentityPub)
	fmt.Printf("acl         %d live links, %d grants, %d revocations\n", st.Shares, st.Grants, st.Revocations)
	if st.RevokeDurable {
		fmt.Printf("revoke      DURABLE at %s\n", firstNonEmpty(st.Objstore, "the configured object store"))
	} else {
		fmt.Printf("revoke      ✗ NOT DURABLE — no object store, so every revoke will be refused (F2 cannot pass)\n")
	}
	// The revocation log is the audit trail; showing the tail makes "prove the
	// revoke stuck" a single command.
	if rawRev, err := c.doormanGet(base, "/v1/revocations"); err == nil {
		var revs []doorman.Revocation
		if json.Unmarshal(rawRev, &revs) == nil && len(revs) > 0 {
			fmt.Println("\nrecent revocations:")
			for i, r := range revs {
				if i >= 5 {
					break
				}
				fmt.Printf("  %s  %-8s %-16s %s\n", r.At.Format("2006-01-02 15:04"), r.Kind,
					r.App, firstNonEmpty(r.Subject, r.Share))
			}
		}
	}
	return nil
}

func (c *client) doormanGet(base, path string) ([]byte, error) {
	c.http.Timeout = 30 * time.Second
	resp, err := c.http.Get(base + path)
	if err != nil {
		return nil, doormanUnreachable(base, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: %s", resp.Status, apiError(raw))
	}
	return raw, nil
}

func (c *client) doormanPost(base, path string, body []byte) ([]byte, error) {
	c.http.Timeout = time.Minute
	resp, err := c.http.Post(base+path, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return nil, doormanUnreachable(base, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: %s", resp.Status, apiError(raw))
	}
	return raw, nil
}

func doormanUnreachable(base string, err error) error {
	return fmt.Errorf("cannot reach krill-doorman at %s: %w\n"+
		"  (the sharing verbs talk to the doorman, not krilld — set --doorman or KRILL_DOORMAN,\n"+
		"   and remember the SSH tunnel needs to forward its port too)", base, err)
}

func apiError(raw []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error != "" {
		return body.Error
	}
	return strings.TrimSpace(string(raw))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// shareCounts is the fan-out half of `krill apps`: the doorman knows about
// sharing and krilld does not, so the one command that lists apps asks both.
// A doorman that is absent or down costs a column, never the command.
func (c *client) shareCounts() map[string]int {
	base := doormanAddr(c.admin, "")
	prev := c.http.Timeout
	c.http.Timeout = 3 * time.Second
	defer func() { c.http.Timeout = prev }()
	resp, err := c.http.Get(base + "/v1/shares")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil
	}
	raw, _ := io.ReadAll(resp.Body)
	var shares []doorman.Share
	if json.Unmarshal(raw, &shares) != nil {
		return nil
	}
	out := map[string]int{}
	for _, s := range shares {
		if !s.Revoked {
			out[s.App]++
		}
	}
	return out
}
