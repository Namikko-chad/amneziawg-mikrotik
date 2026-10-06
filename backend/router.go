package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/logbuf"
	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/routeros"
	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/routing"
	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/tunnel"
)

// routerAPI manages the router's routing rules through the RouterOS REST API. The router
// credentials come with every request (the web UI keeps them for the browser session) and are
// never written to disk; only the plan is.
type routerAPI struct {
	path string
	logs *logbuf.Buffer
	mu   sync.Mutex // serializes changes to the router and the state file
}

// routerState is stored in router.json.
type routerState struct {
	Plan     routing.Plan      `json:"plan"`
	SavedDNS *routing.SavedDNS `json:"saved_dns,omitempty"`
}

// routerAuth is sent by the web UI with every router request.
type routerAuth struct {
	URL      string `json:"url"`
	User     string `json:"user"`
	Password string `json:"password"`
	Insecure bool   `json:"insecure"`
}

func registerRouter(mux *http.ServeMux, dataDir string, logs *logbuf.Buffer) {
	r := &routerAPI{path: filepath.Join(dataDir, "router.json"), logs: logs}
	mux.HandleFunc("GET /api/router", r.handleGet)
	mux.HandleFunc("POST /api/router/info", r.handleInfo)
	mux.HandleFunc("POST /api/router/apply", r.handleApply)
}

func (r *routerAPI) load() (routerState, error) {
	st := routerState{Plan: routing.Plan{Mode: routing.Off}}
	b, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

func (r *routerAPI) save(st routerState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

func (r *routerAPI) handleGet(w http.ResponseWriter, _ *http.Request) {
	st, err := r.load()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	url := ""
	if gw, err := defaultGateway(); err == nil {
		url = "http://" + gw.String()
	}
	writeJSON(w, http.StatusOK, map[string]any{"default_url": url, "plan": st.Plan, "dns_saved": st.SavedDNS != nil})
}

// connect checks the credentials and works out how the router sees the container.
func (r *routerAPI) connect(ctx context.Context, a routerAuth) (*routeros.Client, routing.Info, routing.Env, error) {
	if a.URL == "" {
		if gw, err := defaultGateway(); err == nil {
			a.URL = "http://" + gw.String()
		}
	}
	if strings.TrimSpace(a.User) == "" {
		return nil, routing.Info{}, routing.Env{}, errors.New("enter the router user")
	}
	c, err := routeros.New(a.URL, a.User, a.Password, a.Insecure)
	if err != nil {
		return nil, routing.Info{}, routing.Env{}, err
	}
	self, err := localAddrTo(c.Host())
	if err != nil {
		return nil, routing.Info{}, routing.Env{}, err
	}
	info, env, err := routing.Inspect(ctx, c, self)
	env.Probe = netip.MustParseAddr(tunnel.ProbeAddr)
	return c, info, env, err
}

func (r *routerAPI) handleInfo(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Auth routerAuth `json:"auth"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	_, info, _, err := r.connect(ctx, body.Auth)
	if err != nil {
		writeErr(w, routerErrCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (r *routerAPI) handleApply(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Auth routerAuth   `json:"auth"`
		Plan routing.Plan `json:"plan"`
	}
	if !readJSON(w, req, &body) {
		return
	}
	if err := body.Plan.Normalize(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.load()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 60*time.Second)
	defer cancel()
	c, _, env, err := r.connect(ctx, body.Auth)
	if err != nil {
		writeErr(w, routerErrCode(err), err.Error())
		return
	}

	r.logs.Printf("router: applying plan (mode %s)", body.Plan.Mode)
	saved, applyErr := routing.Apply(ctx, c, body.Plan, env, st.SavedDNS, r.logs.Printf)
	// The DNS setup to restore must be kept even if a later step failed.
	st.SavedDNS = saved
	if applyErr == nil {
		st.Plan = body.Plan
	}
	if err := r.save(st); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if applyErr != nil {
		r.logs.Printf("router: %v", applyErr)
		writeErr(w, routerErrCode(applyErr), applyErr.Error())
		return
	}
	_, info, _, err := r.connect(ctx, body.Auth)
	if err != nil {
		writeErr(w, routerErrCode(err), "applied, but "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan": st.Plan, "info": info})
}

func routerErrCode(err error) int {
	var re *routeros.Error
	if errors.As(err, &re) && re.Status == http.StatusUnauthorized {
		return http.StatusUnauthorized
	}
	return http.StatusBadGateway
}

// localAddrTo returns the container address the router sees when we connect to host.
func localAddrTo(host string) (netip.Addr, error) {
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(strings.Trim(host, "[]"), "80")
	}
	conn, err := net.Dial("udp4", host) // no packets are sent
	if err != nil {
		return netip.Addr{}, fmt.Errorf("cannot reach router %s: %w", host, err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap(), nil
}

// defaultGateway reads the IPv4 default gateway from /proc/net/route: on RouterOS it is the
// router's end of the veth.
func defaultGateway() (netip.Addr, error) {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return netip.Addr{}, err
	}
	return parseDefaultGateway(string(b))
}

func parseDefaultGateway(table string) (netip.Addr, error) {
	for _, line := range strings.Split(table, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		gw, err := hex.DecodeString(f[2])
		if err != nil || len(gw) != 4 {
			continue
		}
		// Little-endian on every architecture we build for.
		a := netip.AddrFrom4([4]byte{gw[3], gw[2], gw[1], gw[0]})
		if !a.IsUnspecified() {
			return a, nil
		}
	}
	return netip.Addr{}, errors.New("no default gateway")
}
