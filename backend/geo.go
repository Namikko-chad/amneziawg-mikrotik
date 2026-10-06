package main

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/geoip"
	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/routing"
)

// geoAPI edits the country list and serves it to the router as a RouterOS script.
type geoAPI struct {
	store *geoip.Store
}

func registerGeo(mux *http.ServeMux, store *geoip.Store) {
	g := &geoAPI{store: store}
	mux.HandleFunc("GET /api/geo", g.handleGet)
	mux.HandleFunc("POST /api/geo", g.handleSet)
	mux.HandleFunc("POST /api/geo/update", g.handleUpdate)
	mux.HandleFunc("GET "+routing.GeoPath, g.handleRSC)
}

func (g *geoAPI) state() map[string]any {
	cfg := g.store.Config()
	url := ""
	if gw, err := defaultGateway(); err == nil {
		if self, err := localAddrTo(gw.String()); err == nil {
			url = "http://" + self.String() + routing.GeoPath
		}
	}
	return map[string]any{
		"countries":      cfg.Countries,
		"source":         cfg.Source,
		"default_source": geoip.DefaultSource,
		"status":         g.store.Status(),
		"subnets":        len(g.store.Subnets()),
		"url":            url,
		"path":           routing.GeoPath,
		"list":           routing.ListGeo,
	}
}

func (g *geoAPI) handleGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, g.state())
}

func (g *geoAPI) handleSet(w http.ResponseWriter, r *http.Request) {
	var cfg geoip.Config
	if !readJSON(w, r, &cfg) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Second)
	defer cancel()
	if err := g.store.SetConfig(ctx, cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, g.state())
}

func (g *geoAPI) handleUpdate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 100*time.Second)
	defer cancel()
	g.store.Update(ctx, true)
	writeJSON(w, http.StatusOK, g.state())
}

// handleRSC serves the script the router imports. Without any subnets it fails, so the router
// keeps its previous list instead of emptying it.
func (g *geoAPI) handleRSC(w http.ResponseWriter, _ *http.Request) {
	subnets := g.store.Subnets()
	if len(subnets) == 0 {
		http.Error(w, "no country list yet", http.StatusServiceUnavailable)
		return
	}
	body := geoip.RSC(routing.ListGeo, g.store.Config().Countries, subnets)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}
