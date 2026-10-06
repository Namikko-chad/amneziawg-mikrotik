// awg-manager is the HTTP backend that manages the AmneziaWG client inside the container.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/conf"
	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/geoip"
	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/logbuf"
	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/profiles"
	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/sshprov"
	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/tunnel"
	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/vpnkey"
)

type server struct {
	dataDir string
	tun     *tunnel.Manager
	logs    *logbuf.Buffer
	store   *profiles.Store
	mu      sync.Mutex // serializes changes to the active profile and the tunnel
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	logs := logbuf.New(300)
	s := &server{
		dataDir: env("AWG_DATA_DIR", "/etc/amnezia"),
		tun:     tunnel.New(logs, os.Getenv("LAN_IFACE"), strings.Fields(env("CHECK_HOSTS", "1.1.1.1 8.8.8.8"))),
		logs:    logs,
	}
	if err := os.MkdirAll(s.dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	store, err := profiles.Open(filepath.Join(s.dataDir, "profiles.json"), filepath.Join(s.dataDir, tunnel.Iface+".conf"))
	if err != nil {
		log.Fatal(err)
	}
	s.store = store
	geo, err := geoip.Open(filepath.Join(s.dataDir, "geo"), logs.Printf)
	if err != nil {
		log.Fatal(err)
	}

	if !s.disabled() {
		if primary, backup, err := s.configs(); err == nil {
			logs.Printf("starting tunnel from saved config")
			go s.tun.Up(primary, backup)
		}
	}

	mux := http.NewServeMux()
	registerRouter(mux, s.dataDir, logs)
	registerGeo(mux, geo)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("POST /api/config/text", s.handleText)
	mux.HandleFunc("POST /api/config/vpnkey", s.handleVPNKey)
	mux.HandleFunc("POST /api/config/ssh", s.handleSSH)
	mux.HandleFunc("GET /api/profiles", s.handleProfiles)
	mux.HandleFunc("GET /api/profiles/{id}/config", s.handleProfileConfig)
	mux.HandleFunc("POST /api/profiles/{id}/activate", s.handleActivate)
	mux.HandleFunc("PATCH /api/profiles/{id}", s.handleRename)
	mux.HandleFunc("DELETE /api/profiles/{id}", s.handleDelete)
	mux.HandleFunc("POST /api/backup", s.handleBackup)
	mux.HandleFunc("POST /api/up", s.handleUp)
	mux.HandleFunc("POST /api/down", s.handleDown)
	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"lines": logs.Lines()})
	})

	srv := &http.Server{Addr: env("LISTEN", "127.0.0.1:8080"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go geo.Run(ctx)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	logs.Printf("listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	s.tun.Down()
}

func (s *server) disabledPath() string { return filepath.Join(s.dataDir, "disabled") }

func (s *server) disabled() bool {
	_, err := os.Stat(s.disabledPath())
	return err == nil
}

func (s *server) setDisabled(v bool) {
	if v {
		os.WriteFile(s.disabledPath(), nil, 0o600)
	} else {
		os.Remove(s.disabledPath())
	}
}

// loadConfig parses the active profile.
func (s *server) loadConfig() (*conf.Config, error) {
	p, err := s.store.Active()
	if err != nil {
		return nil, err
	}
	return conf.Parse(p.Config)
}

// configs parses the active profile and the backup (nil if none is set or it is unusable).
func (s *server) configs() (primary, backup *conf.Config, err error) {
	if primary, err = s.loadConfig(); err != nil {
		return nil, nil, err
	}
	if b, err := s.store.Backup(); err == nil {
		if backup, err = conf.Parse(b.Config); err != nil {
			s.logs.Printf("backup profile %q: %v", b.Name, err)
			backup = nil
		}
	}
	return primary, backup, nil
}

// restart brings the tunnel up with the current active and backup profiles.
func (s *server) restart() error {
	primary, backup, err := s.configs()
	if err != nil {
		return err
	}
	return s.tun.Up(primary, backup)
}

// apply validates a new config, saves it as a new profile, makes it active and starts it.
func (s *server) apply(w http.ResponseWriter, text, source, name string) {
	c, err := conf.Parse(text)
	if err == nil {
		err = c.Validate()
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid config: "+err.Error())
		return
	}
	if strings.TrimSpace(name) == "" {
		name = endpointHost(c)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.store.Add(name, source, c.String(), true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logs.Printf("profile %q saved (source: %s)", p.Name, source)
	s.setDisabled(false)
	if err := s.restart(); err != nil {
		writeErr(w, http.StatusBadGateway, "config saved, but tunnel failed: "+err.Error())
		return
	}
	s.handleStatus(w, nil)
}

func endpointHost(c *conf.Config) string {
	p := c.Peers()
	if len(p) == 0 {
		return ""
	}
	ep := p[0].Get("Endpoint")
	if h, _, err := net.SplitHostPort(ep); err == nil {
		return h
	}
	return ep
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

func (s *server) handleText(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Config string `json:"config"`
		Name   string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	s.apply(w, req.Config, "text", req.Name)
}

func (s *server) handleVPNKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key        string `json:"key"`
		ClientName string `json:"client_name"`
		Name       string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	res, err := vpnkey.Decode(req.Key)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if res.Server != nil {
		// Full-access key: provision a fresh client with the embedded credentials.
		s.logs.Printf("key contains server credentials, provisioning a new client over SSH")
		s.provision(w, r.Context(), sshprov.Request{
			Host: res.Server.Host, Port: res.Server.Port, User: res.Server.User,
			Password: res.Server.Password, ClientName: req.ClientName,
		}, "vpn:// (ssh)", firstNonEmpty(req.Name, res.Name, res.Server.Host))
		return
	}
	s.apply(w, res.Config, "vpn://", firstNonEmpty(req.Name, res.Name))
}

func (s *server) handleSSH(w http.ResponseWriter, r *http.Request) {
	var req struct {
		sshprov.Request
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	s.provision(w, r.Context(), req.Request, "ssh", firstNonEmpty(req.Name, req.Host))
}

func (s *server) provision(w http.ResponseWriter, ctx context.Context, req sshprov.Request, source, name string) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	text, err := sshprov.Provision(ctx, req, s.logs.Printf)
	if err != nil {
		s.logs.Printf("ssh: %v", err)
		writeErr(w, http.StatusBadGateway, "ssh: "+err.Error())
		return
	}
	s.apply(w, text, source, name)
}

func (s *server) handleUp(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.store.Active(); err != nil {
		writeErr(w, http.StatusBadRequest, "no saved config")
		return
	}
	s.setDisabled(false)
	if err := s.restart(); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.handleStatus(w, nil)
}

func (s *server) handleDown(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setDisabled(true)
	s.tun.Down()
	s.handleStatus(w, nil)
}

func (s *server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	resp := map[string]any{"running": s.tun.Running(), "configured": false, "enabled": !s.disabled()}
	active, err := s.store.Active()
	if err == nil {
		resp["configured"] = true
		resp["profile"] = map[string]string{"id": active.ID, "name": active.Name}
	}
	backup, err := s.store.Backup()
	if err == nil {
		resp["backup"] = map[string]string{"id": backup.ID, "name": backup.Name}
	}

	// Top-level address/endpoint/peers describe the link that carries traffic right now.
	inUse := active
	type linkInfo struct {
		tunnel.LinkStatus
		ProfileID string `json:"profile_id"`
	}
	var links []linkInfo
	var peers []tunnel.PeerStatus
	for _, l := range s.tun.Status() {
		li := linkInfo{LinkStatus: l, ProfileID: active.ID}
		if l.Backup {
			li.ProfileID = backup.ID
		}
		if l.InUse {
			peers = l.Peers
			if l.Error != "" {
				resp["error"] = l.Error
			}
			if l.Backup {
				inUse = backup
				resp["using"] = "backup"
			} else {
				resp["using"] = "primary"
			}
		}
		links = append(links, li)
	}
	if c, err := conf.Parse(inUse.Config); err == nil && inUse.ID != "" {
		if i := c.Interface(); i != nil {
			resp["address"] = strings.Join(i.All("Address"), ", ")
		}
		if p := c.Peers(); len(p) > 0 {
			resp["endpoint"] = p[0].Get("Endpoint")
		}
	}
	resp["peers"] = peers
	resp["links"] = links
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	c, err := s.loadConfig()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"config": ""})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": c.Masked()})
}

type profileInfo struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Source   string    `json:"source"`
	Created  time.Time `json:"created"`
	Endpoint string    `json:"endpoint"`
	Address  string    `json:"address"`
}

func (s *server) handleProfiles(w http.ResponseWriter, _ *http.Request) {
	list, active := s.store.List()
	backup, _ := s.store.Backup()
	out := make([]profileInfo, 0, len(list))
	for _, p := range list {
		pi := profileInfo{ID: p.ID, Name: p.Name, Source: p.Source, Created: p.Created}
		if c, err := conf.Parse(p.Config); err == nil {
			if i := c.Interface(); i != nil {
				pi.Address = strings.Join(i.All("Address"), ", ")
			}
			if peers := c.Peers(); len(peers) > 0 {
				pi.Endpoint = peers[0].Get("Endpoint")
			}
		}
		out = append(out, pi)
	}
	writeJSON(w, http.StatusOK, map[string]any{"active": active, "backup": backup.ID, "profiles": out})
}

func (s *server) handleProfileConfig(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	c, err := conf.Parse(p.Config)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": c.Masked()})
}

// handleActivate selects a profile. If the tunnel is meant to be up, it reconnects to it.
// Selecting the backup swaps the roles of the two profiles.
func (s *server) handleActivate(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.store.SetActive(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	s.logs.Printf("switched to profile %q", p.Name)
	if !s.disabled() {
		if err := s.restart(); err != nil {
			writeErr(w, http.StatusBadGateway, "switched, but tunnel failed: "+err.Error())
			return
		}
	}
	s.handleStatus(w, nil)
}

func (s *server) handleRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.store.Rename(r.PathValue("id"), req.Name); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, profiles.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// handleDelete removes a profile. Deleting the active one promotes the backup and reconnects
// to it; without a backup the tunnel stops rather than silently sending traffic through another
// server.
func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	backup, _ := s.store.Backup()
	wasActive, err := s.store.Delete(id)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, profiles.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, err.Error())
		return
	}
	switch {
	case wasActive && backup.ID != "" && !s.disabled():
		if err := s.restart(); err != nil {
			writeErr(w, http.StatusBadGateway, "deleted, but backup failed: "+err.Error())
			return
		}
	case wasActive:
		s.setDisabled(true)
		s.tun.Down()
	case id == backup.ID && !s.disabled() && s.tun.Running():
		if err := s.restart(); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	s.handleStatus(w, nil)
}

// handleBackup sets ({"id": "..."}) or clears ({"id": ""}) the backup profile and restarts the
// tunnel if it is up.
func (s *server) handleBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.SetBackup(req.ID); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, profiles.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, err.Error())
		return
	}
	if req.ID == "" {
		s.logs.Printf("backup cleared")
	} else {
		s.logs.Printf("backup set")
	}
	if !s.disabled() && s.tun.Running() {
		if err := s.restart(); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	s.handleStatus(w, nil)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
