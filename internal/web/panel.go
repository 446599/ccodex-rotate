// Package web serves a minimal status/control panel for ccodex-rotate.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"time"

	"ccodex-rotate/internal/mihomo"
	"ccodex-rotate/internal/proxy"
)

// Panel ties together the manager, egress and proxy for the UI.
type Panel struct {
	Listen           string
	Upstream         string
	ProbeModel       string
	TargetLengths    []int
	SuccessIntervalS int
	RetryIntervalS   int
	Trigger          func()
	SourcesAdd       func(kind string, lines []string) (int, error)
	SourcesClear     func(kind string) error
	SourcesCounts    func() (subs, nodes, proxies int)
	Mgr              *mihomo.Manager
	Eg               *mihomo.Egress
	Proxy            *proxy.Server
}

// Handler builds the HTTP routes for the panel and JSON API.
func (p *Panel) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/panel/assets/", http.StripPrefix("/panel/", http.FileServer(http.FS(panelAssets))))
	mux.HandleFunc("/panel/", p.page)
	mux.HandleFunc("/panel", p.page)
	mux.HandleFunc("/api/status", p.status)
	mux.HandleFunc("/api/nodes", p.nodes)
	mux.HandleFunc("/api/rotate", p.rotate)
	mux.HandleFunc("/api/collect", p.collect)
	mux.HandleFunc("/api/collect/stop", p.collectStop)
	mux.HandleFunc("/api/scan", p.collect)
	mux.HandleFunc("/api/injection", p.injection)
	mux.HandleFunc("/api/force-model", p.forceModel)
	mux.HandleFunc("/api/sources/add", p.sourcesAdd)
	mux.HandleFunc("/api/sources/clear", p.sourcesClear)
	mux.HandleFunc("/api/pin", p.pin)
	mux.HandleFunc("/api/reset", p.reset)
	mux.HandleFunc("/api/recheck", p.recheck)
	return mux
}

func (p *Panel) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tpl.Execute(w, map[string]any{
		"Listen":   p.Listen,
		"Upstream": p.Upstream,
	})
}

func (p *Panel) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	node, _ := p.Mgr.Current(ctx)
	ok, reachable, unknown, failed, total := p.Eg.Counts()
	lastCollect, lastCollectOK, nextCollect, collecting := p.Eg.CollectInfo()
	ctried, ctotal := p.Eg.CollectProgress()
	seenModel, seenLen := p.Eg.LastSeen()
	reqs, errs, recent := p.Proxy.Stats()
	m := map[string]any{
		"listen":           p.Listen,
		"upstream":         p.Upstream,
		"node":             node,
		"manual":           p.Eg.Manual(),
		"alive":            ok + reachable + unknown,
		"ok":               ok,
		"reachable":        reachable,
		"unknown":          unknown,
		"failed":           failed,
		"total":            total,
		"collecting":       collecting,
		"collect_tried":    ctried,
		"collect_total":    ctotal,
		"last_seen_model":  seenModel,
		"last_seen_len":    seenLen,
		"inject":           p.Proxy.InjectionEnabled(),
		"force_model":      p.Proxy.ForceModel(),
		"auth_ready":       p.Proxy.HasAuth(),
		"last_collect":     lastCollect,
		"last_collect_ok":  lastCollectOK,
		"next_collect":     nextCollect,
		"probe_model":      p.ProbeModel,
		"target_lengths":   p.TargetLengths,
		"success_interval": p.SuccessIntervalS,
		"retry_interval":   p.RetryIntervalS,
		"requests":         reqs,
		"errors":           errs,
		"recent":           recent,
		"collect_log":      p.Eg.CollectLog(),
		"states":           p.Proxy.StateSnapshot(),
		"state_ttl":        p.Proxy.StateTTLSeconds(),
		"mihomo_error":     errString(p.Mgr.Err()),
	}
	if p.SourcesCounts != nil {
		s, n, px := p.SourcesCounts()
		m["subs"] = s
		m["nodes"] = n
		m["proxies"] = px
	}
	writeJSON(w, m)
}

func (p *Panel) nodes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	typeByName := map[string]string{}
	if ns, err := p.Mgr.Nodes(ctx); err == nil {
		for _, n := range ns {
			typeByName[n.Name] = n.Type
		}
	}
	entries := p.Eg.Snapshot()
	nodes := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		alive := e.State != "failed"
		nodes = append(nodes, map[string]any{
			"name":   e.Name,
			"type":   typeByName[e.Name],
			"delay":  e.Delay,
			"alive":  alive,
			"state":  e.State,
			"reason": e.Reason,
		})
	}
	writeJSON(w, map[string]any{"current": p.Eg.Current(), "nodes": nodes})
}

func (p *Panel) rotate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	node, changed := p.Eg.Rotate(ctx, "")
	writeJSON(w, map[string]any{"node": node, "changed": changed})
}

func (p *Panel) collect(w http.ResponseWriter, r *http.Request) {
	// Trigger the collection loop (or run a one-off collection if not wired).
	if p.Trigger != nil {
		p.Trigger()
	} else {
		go p.Eg.Collect(context.Background(), p.ProbeModel)
	}
	writeJSON(w, map[string]any{"started": true})
}

func (p *Panel) collectStop(w http.ResponseWriter, r *http.Request) {
	stopped := p.Eg.StopCollect()
	writeJSON(w, map[string]any{"stopped": stopped})
}

func (p *Panel) forceModel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.Enabled {
		p.Proxy.SetForceModel(p.ProbeModel)
	} else {
		p.Proxy.SetForceModel("")
	}
	writeJSON(w, map[string]any{"force_model": p.Proxy.ForceModel()})
}

func (p *Panel) injection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	p.Proxy.SetInjection(body.Enabled)
	writeJSON(w, map[string]any{"injection": body.Enabled})
}

func (p *Panel) sourcesAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind  string   `json:"kind"`
		Lines []string `json:"lines"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Kind == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if p.SourcesAdd == nil {
		writeJSON(w, map[string]any{"error": "not supported"})
		return
	}
	added, err := p.SourcesAdd(body.Kind, body.Lines)
	resp := map[string]any{"added": added}
	if err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, resp)
}

func (p *Panel) sourcesClear(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Kind == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if p.SourcesClear == nil {
		writeJSON(w, map[string]any{"error": "not supported"})
		return
	}
	if err := p.SourcesClear(body.Kind); err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (p *Panel) pin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, "need name", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := p.Eg.PinUser(ctx, body.Name); err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"node": body.Name})
}

func (p *Panel) reset(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := p.Eg.Reset(ctx); err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"node": "AUTO"})
}

func (p *Panel) recheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	_ = p.Mgr.Recheck(ctx)
	writeJSON(w, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var tpl = template.Must(template.New("panel").Parse(pageHTML))

//go:embed assets/panel.html
var pageHTML string

//go:embed assets/panel.css assets/panel.js
var panelAssets embed.FS
