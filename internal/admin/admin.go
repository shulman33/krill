// Package admin is krilld's control API. It binds to loopback by default:
// anything that can reach it can register and delete apps.
//
//	POST   /v1/apps                {name, golden, vcpus?, mem_mib?, guest_port?, kernel?, boot_args?}
//	GET    /v1/apps
//	GET    /v1/apps/{name}
//	POST   /v1/apps/{name}/wake
//	POST   /v1/apps/{name}/freeze
//	DELETE /v1/apps/{name}
//	GET    /healthz
//
// (Hand-rolled routing: Go 1.21 ServeMux has no method/wildcard patterns.)
package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/shulman33/krill/internal/lifecycle"
	"github.com/shulman33/krill/internal/registry"
)

type Server struct {
	sup *lifecycle.Supervisor
	log *slog.Logger
}

func New(sup *lifecycle.Supervisor, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{sup: sup, log: log}
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
	default:
		http.NotFound(w, r)
	}
}

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
	default:
		http.NotFound(w, r)
	}
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
