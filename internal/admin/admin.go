// Package admin is krilld's control API. It binds to loopback by default:
// anything that can reach it can register, deploy and delete apps.
//
//	POST   /v1/apps                {name, golden, vcpus?, mem_mib?, guest_port?, kernel?, boot_args?}
//	GET    /v1/apps
//	GET    /v1/apps/{name}
//	POST   /v1/apps/{name}/deploy  body: tar.gz build context (M2 deploy path)
//	GET    /v1/apps/{name}/logs    ?tail=N — serial tail + structured errors
//	POST   /v1/apps/{name}/wake
//	POST   /v1/apps/{name}/freeze
//	DELETE /v1/apps/{name}
//	GET    /v1/objstore            live durability check (a real CAS round trip)
//	GET    /v1/registry/backups    registry snapshots shipped off-box
//	POST   /v1/registry/backup     take one now
//	GET    /healthz
//
// (Hand-rolled routing: Go 1.21 ServeMux has no method/wildcard patterns.)
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shulman33/krill/internal/builder"
	"github.com/shulman33/krill/internal/guestlog"
	"github.com/shulman33/krill/internal/lifecycle"
	"github.com/shulman33/krill/internal/objstore"
	"github.com/shulman33/krill/internal/regbackup"
	"github.com/shulman33/krill/internal/registry"
)

// maxContextBytes bounds an uploaded build context. Small software.
const maxContextBytes = 512 << 20

// ImageBuilder is what the deploy endpoint needs from internal/builder;
// tests substitute a fake so no docker is required.
type ImageBuilder interface {
	Build(ctx context.Context, name, contextDir string, sizeMB int) (*builder.Result, error)
}

// DeployConfig is the slice of daemon config the deploy path uses.
type DeployConfig struct {
	// WorkDir holds uploaded build contexts while they build.
	WorkDir string
	// BaseHost and RouterAddr shape the printed URL:
	// http://<app>.<BaseHost>:<router port>/
	BaseHost   string
	RouterAddr string
	// BuildTimeout bounds one build; VerifyTimeout bounds the post-deploy
	// wake that proves the app actually boots.
	BuildTimeout  time.Duration
	VerifyTimeout time.Duration
}

type Server struct {
	sup *lifecycle.Supervisor
	bld ImageBuilder
	dep DeployConfig
	dur Durability
	log *slog.Logger
}

func New(sup *lifecycle.Supervisor, bld ImageBuilder, dep DeployConfig, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	if dep.VerifyTimeout == 0 {
		dep.VerifyTimeout = 60 * time.Second
	}
	return &Server{sup: sup, bld: bld, dep: dep, log: log}
}

type registerReq struct {
	Name      string `json:"name"`
	Golden    string `json:"golden"` // host path to the golden ext4 image
	VCPUs     int    `json:"vcpus"`
	MemMiB    int    `json:"mem_mib"`
	GuestPort int    `json:"guest_port"`
	Kernel    string `json:"kernel"`
	BootArgs  string `json:"boot_args"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case path == "/healthz" && r.Method == http.MethodGet:
		w.Write([]byte("ok\n"))
	case path == "/v1/apps" && r.Method == http.MethodPost:
		s.register(w, r)
	case path == "/v1/apps" && r.Method == http.MethodGet:
		s.list(w)
	case strings.HasPrefix(path, "/v1/apps/"):
		s.appRoute(w, r, strings.TrimPrefix(path, "/v1/apps/"))
	case path == "/v1/objstore" && r.Method == http.MethodGet:
		s.objstoreCheck(w, r)
	case path == "/v1/registry/backups" && r.Method == http.MethodGet:
		s.listBackups(w, r)
	case path == "/v1/registry/backup" && r.Method == http.MethodPost:
		s.takeBackup(w, r)
	default:
		http.NotFound(w, r)
	}
}

// Durability is the optional durability surface: the object store behind the
// data plane and the registry-backup manager. Set by the daemon after
// construction (both are nil when --data-plane is off).
type Durability struct {
	// Spec is the objstore URL as configured, for display.
	Spec  string
	Store objstore.Store
	// BackupSpec is where registry snapshots go (may differ from Spec).
	BackupSpec string
	Backups    *regbackup.Manager
}

func (s *Server) SetDurability(d Durability) { s.dur = d }

// objstoreCheck answers "is durability actually working right now" with a
// live conditional-PUT round trip rather than a cached opinion.
func (s *Server) objstoreCheck(w http.ResponseWriter, r *http.Request) {
	if s.dur.Store == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("no object store: the data plane is off"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out := map[string]any{"spec": s.dur.Spec, "backup_spec": s.dur.BackupSpec}
	code := http.StatusOK
	if err := objstore.Check(ctx, s.dur.Store); err != nil {
		out["ok"], out["error"] = false, err.Error()
		code = http.StatusServiceUnavailable
	} else {
		out["ok"] = true
	}
	// After the check, not before: that is when credentials have resolved and
	// the description can name the identity we are writing as.
	if d, ok := s.dur.Store.(interface{ Describe() string }); ok {
		out["store"] = d.Describe()
	}
	reply(w, code, out)
}

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request) {
	if s.dur.Backups == nil {
		fail(w, http.StatusServiceUnavailable, errBackupsOff)
		return
	}
	list, err := s.dur.Backups.List(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	if list == nil {
		list = []regbackup.Info{}
	}
	reply(w, http.StatusOK, list)
}

func (s *Server) takeBackup(w http.ResponseWriter, r *http.Request) {
	if s.dur.Backups == nil {
		fail(w, http.StatusServiceUnavailable, errBackupsOff)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	info, err := s.dur.Backups.RunOnce(ctx)
	if err != nil {
		s.log.Error("registry backup failed", "err", err)
		fail(w, http.StatusBadGateway, err)
		return
	}
	s.log.Info("registry backed up on request", "key", info.Key, "bytes", info.Bytes)
	reply(w, http.StatusOK, info)
}

var errBackupsOff = errors.New("registry backups are disabled (--registry-backup-interval 0)")

func (s *Server) appRoute(w http.ResponseWriter, r *http.Request, rest string) {
	name, action, _ := strings.Cut(rest, "/")
	switch {
	case action == "" && r.Method == http.MethodGet:
		s.status(w, name)
	case action == "" && r.Method == http.MethodDelete:
		s.delete(w, name)
	case action == "wake" && r.Method == http.MethodPost:
		s.wake(w, r, name)
	case action == "freeze" && r.Method == http.MethodPost:
		s.freeze(w, name)
	case action == "deploy" && r.Method == http.MethodPost:
		s.deploy(w, r, name)
	case action == "logs" && r.Method == http.MethodGet:
		s.logs(w, r, name)
	case action == "stream" && r.Method == http.MethodGet:
		s.stream(w, r, name)
	case action == "data" && r.Method == http.MethodGet:
		s.exportData(w, r, name)
	case action == "restore" && r.Method == http.MethodPost:
		s.restore(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

// stream exposes the app's data-plane manifest: head LSN, current epoch,
// segments, checkpoints, seals, branches.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, name string) {
	m, err := s.sup.StreamStatus(r.Context(), name)
	if err != nil {
		fail(w, statusFor(err), err)
		return
	}
	reply(w, http.StatusOK, m)
}

// exportData serves the app's SQLite database as one checkpointed file. It
// is what the doorman's "data" share plane relays, so an ordinary person with
// a data link can download the thing their app actually wrote — read out of
// the guest's disk by the host, with no cooperation from the guest.
func (s *Server) exportData(w http.ResponseWriter, r *http.Request, name string) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	db, err := s.sup.ExportData(ctx, name)
	if err != nil {
		fail(w, statusFor(err), err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".db"))
	w.Write(db)
}

// restore is PITR: {"at_lsn": N} or {"at_time": "RFC3339"} → branch the
// stream at that point and rebuild the app's data disk to it. The old
// stream stays intact forever (D4) — a follow-up restore can return to it.
func (s *Server) restore(w http.ResponseWriter, r *http.Request, name string) {
	var req struct {
		AtLSN  uint64 `json:"at_lsn"`
		AtTime string `json:"at_time"`
		// Stream optionally names the branch source; "" = the app's current
		// stream. Restoring back past an earlier restore = name the ancestor.
		Stream string `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad restore request: "+err.Error(), http.StatusBadRequest)
		return
	}
	var at time.Time
	if req.AtTime != "" {
		var err error
		if at, err = time.Parse(time.RFC3339, req.AtTime); err != nil {
			http.Error(w, "at_time must be RFC3339: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	stream, lsn, err := s.sup.RestoreData(r.Context(), name, req.Stream, req.AtLSN, at)
	if err != nil {
		fail(w, statusFor(err), err)
		return
	}
	reply(w, http.StatusOK, map[string]any{
		"app": name, "stream": stream, "lsn": lsn,
		"note": "app is COLD on the new branch; next request boots it. The previous stream is untouched and restorable.",
	})
}

// deployResp is the deploy tool's whole feedback loop in one payload: where
// the app lives, whether it actually boots, and if not, why.
type deployResp struct {
	App     lifecycle.Status `json:"app"`
	URL     string           `json:"url"`
	Created bool             `json:"created"`
	// Ready reports the post-deploy verification wake: the app booted and
	// accepted a TCP connection. false + Errors = the self-heal loop.
	Ready     bool             `json:"ready"`
	WakeError string           `json:"wake_error,omitempty"`
	Errors    []guestlog.Error `json:"errors,omitempty"`
	SizeMB    int              `json:"size_mb"`
	Warnings  []string         `json:"warnings,omitempty"`
	BuildSecs float64          `json:"build_secs"`
	CurlHint  string           `json:"curl_hint"`
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request, name string) {
	if !registry.ValidName(name) {
		fail(w, http.StatusBadRequest, fmt.Errorf("invalid app name %q (need a DNS label)", name))
		return
	}
	q := r.URL.Query()
	vcpus := intParam(q.Get("vcpus"), 0)
	memMiB := intParam(q.Get("mem_mib"), 0)
	guestPort := intParam(q.Get("guest_port"), 0)
	sizeMB := intParam(q.Get("size_mb"), 0)
	verify := q.Get("verify") != "false"

	ctxDir, err := os.MkdirTemp(s.dep.WorkDir, "ctx-"+name+"-")
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	defer os.RemoveAll(ctxDir)
	body := http.MaxBytesReader(w, r.Body, maxContextBytes)
	if err := builder.ExtractTarGz(body, ctxDir); err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("unpacking build context: %w", err))
		return
	}

	start := time.Now()
	bctx, cancel := context.WithTimeout(r.Context(), s.dep.BuildTimeout)
	defer cancel()
	res, err := s.bld.Build(bctx, name, ctxDir, sizeMB)
	if err != nil {
		var be *builder.BuildError
		if errors.As(err, &be) {
			reply(w, http.StatusUnprocessableEntity, map[string]any{
				"error":     be.Error(),
				"stage":     be.Stage,
				"build_log": be.Log,
			})
			return
		}
		fail(w, http.StatusInternalServerError, err)
		return
	}
	defer res.Cleanup()
	buildSecs := time.Since(start).Seconds()

	// Missing knobs inherit the app's current values on redeploy, defaults on
	// first deploy. Port preference: explicit flag > EXPOSE > previous > 8000.
	prev, prevErr := s.sup.Status(name)
	if vcpus == 0 {
		vcpus = statusOr(prevErr, prev.VCPUs, 1)
	}
	if memMiB == 0 {
		memMiB = statusOr(prevErr, prev.MemMiB, 512)
	}
	if guestPort == 0 {
		guestPort = res.GuestPort
	}
	if guestPort == 0 {
		guestPort = statusOr(prevErr, prev.GuestPort, 8000)
	}

	meta, created, err := s.sup.Deploy(r.Context(), registry.App{
		Name:      name,
		VCPUs:     vcpus,
		MemMiB:    memMiB,
		GuestPort: guestPort,
		BootArgs:  builder.BootArgs,
	}, res.GoldenPath)
	if err != nil {
		fail(w, statusFor(err), err)
		return
	}

	resp := deployResp{
		Created:  created,
		SizeMB:   res.SizeMB,
		Warnings: res.Warnings,
		URL:      s.appURL(meta.Name),
		CurlHint: s.curlHint(meta.Name),
	}
	resp.BuildSecs = float64(int(buildSecs*10)) / 10

	if verify {
		vctx, vcancel := context.WithTimeout(context.Background(), s.dep.VerifyTimeout)
		_, release, err := s.sup.Acquire(vctx, meta.Name)
		vcancel()
		if err == nil {
			release()
			resp.Ready = true
		} else {
			resp.WakeError = err.Error()
			resp.Errors = s.guestErrors(meta.Name)
		}
	}
	resp.App, _ = s.sup.Status(meta.Name)
	code := http.StatusCreated
	if !created {
		code = http.StatusOK
	}
	reply(w, code, resp)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request, name string) {
	tail := intParam(r.URL.Query().Get("tail"), 100)
	path, err := s.sup.SerialLogPath(name)
	if err != nil {
		fail(w, statusFor(err), err)
		return
	}
	// Parse over a wider window than we return: the traceback that matters
	// may sit just above the requested tail.
	window := tail
	if window < 500 {
		window = 500
	}
	lines, err := guestlog.TailFile(path, window)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	errs := guestlog.Parse(lines)
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	reply(w, http.StatusOK, map[string]any{"lines": lines, "errors": errs})
}

func (s *Server) guestErrors(name string) []guestlog.Error {
	path, err := s.sup.SerialLogPath(name)
	if err != nil {
		return nil
	}
	lines, err := guestlog.TailFile(path, 500)
	if err != nil {
		return nil
	}
	return guestlog.Parse(lines)
}

func (s *Server) appURL(name string) string {
	host := name + "." + s.dep.BaseHost
	if p := routerPort(s.dep.RouterAddr); p != "" && p != "80" {
		host = net.JoinHostPort(host, p)
	}
	return "http://" + host + "/"
}

func (s *Server) curlHint(name string) string {
	port := routerPort(s.dep.RouterAddr)
	if port == "" {
		port = "80"
	}
	return fmt.Sprintf("curl -H 'Host: %s.%s' http://<router-host>:%s/", name, s.dep.BaseHost, port)
}

func routerPort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return port
}

func intParam(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func statusOr(err error, val, def int) int {
	if err != nil || val == 0 {
		return def
	}
	return val
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || req.Golden == "" {
		fail(w, http.StatusBadRequest, errors.New("name and golden are required"))
		return
	}
	if req.VCPUs == 0 {
		req.VCPUs = 1
	}
	if req.MemMiB == 0 {
		req.MemMiB = 512
	}
	if req.GuestPort == 0 {
		req.GuestPort = 8000
	}
	meta, err := s.sup.Register(registry.App{
		Name:       req.Name,
		VCPUs:      req.VCPUs,
		MemMiB:     req.MemMiB,
		GuestPort:  req.GuestPort,
		KernelPath: req.Kernel,
		BootArgs:   req.BootArgs,
	}, req.Golden)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	st, _ := s.sup.Status(meta.Name)
	reply(w, http.StatusCreated, st)
}

func (s *Server) list(w http.ResponseWriter) {
	all, err := s.sup.StatusAll()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	reply(w, http.StatusOK, all)
}

func (s *Server) status(w http.ResponseWriter, name string) {
	st, err := s.sup.Status(name)
	if err != nil {
		fail(w, statusFor(err), err)
		return
	}
	reply(w, http.StatusOK, st)
}

func (s *Server) wake(w http.ResponseWriter, r *http.Request, name string) {
	_, release, err := s.sup.Acquire(r.Context(), name)
	if err != nil {
		fail(w, statusFor(err), err)
		return
	}
	release()
	st, _ := s.sup.Status(name)
	reply(w, http.StatusOK, st)
}

func (s *Server) freeze(w http.ResponseWriter, name string) {
	err := s.sup.Freeze(name)
	if err != nil && !errors.Is(err, lifecycle.ErrNotActive) {
		fail(w, statusFor(err), err)
		return
	}
	// ErrNotActive is fine: freeze is idempotent from the caller's seat.
	st, _ := s.sup.Status(name)
	reply(w, http.StatusOK, st)
}

func (s *Server) delete(w http.ResponseWriter, name string) {
	if err := s.sup.Delete(name); err != nil {
		fail(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, lifecycle.ErrBusy):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	reply(w, code, map[string]string{"error": err.Error()})
}
