// Package web serves a minimal status/control panel for ccodex-rotate.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sync"
	"time"

	"ccodex-rotate/internal/mihomo"
	"ccodex-rotate/internal/mtrace"
	"ccodex-rotate/internal/proxy"
)

// Panel ties together the manager, egress and proxy for the UI.
type Panel struct {
	Listen        string
	Upstream      string
	Version       string
	Password      string
	ProbeModel    string
	Trace         *mtrace.Monitor
	TraceModel    string
	SourcesAdd    func(kind string, lines []string) (int, error)
	SourcesClear  func(kind string) error
	SourcesCounts func() (subs, nodes, proxies int)
	Mgr           *mihomo.Manager
	Eg            *mihomo.Egress
	Proxy         *proxy.Server

	mu   sync.Mutex
	subs map[chan string]struct{}
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
	mux.HandleFunc("/api/trace", p.traceToggle)
	mux.HandleFunc("/api/trace-interval", p.traceInterval)
	mux.HandleFunc("/api/trace-now", p.traceNow)
	mux.HandleFunc("/api/events", p.events)
	mux.HandleFunc("/api/force-model", p.forceModel)
	mux.HandleFunc("/api/sources/add", p.sourcesAdd)
	mux.HandleFunc("/api/sources/clear", p.sourcesClear)
	mux.HandleFunc("/api/pin", p.pin)
	mux.HandleFunc("/api/reset", p.reset)
	mux.HandleFunc("/api/recheck", p.recheck)
	return protectPanel(mux, p.Password)
}

func (p *Panel) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tpl.Execute(w, map[string]any{
		"Listen":   p.Listen,
		"Upstream": p.Upstream,
		"Version":  p.Version,
	})
}

func (p *Panel) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	node, _ := p.Mgr.Current(ctx)
	ok, reachable, unknown, failed, total := p.Eg.Counts()
	reqs, errs, recent := p.Proxy.Stats()
	m := map[string]any{
		"listen":         p.Listen,
		"upstream":       p.Upstream,
		"node":           node,
		"manual":         p.Eg.Manual(),
		"alive":          ok + reachable + unknown,
		"ok":             ok,
		"reachable":      reachable,
		"unknown":        unknown,
		"failed":         failed,
		"total":          total,
		"force_model":    p.Proxy.ForceModel(),
		"probe_model":    p.ProbeModel,
		"requests":       reqs,
		"errors":         errs,
		"recent":         recent,
		"trace_enabled":  p.traceEnabled(),
		"trace_running":  p.traceRunning(),
		"trace_interval": p.traceIntervalSec(),
		"trace_last":     p.traceLast(),
		"trace_log":      p.traceLog(), "mihomo_error": errString(p.Mgr.Err()),
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
			"name":     e.Name,
			"type":     typeByName[e.Name],
			"delay":    e.Delay,
			"alive":    alive,
			"state":    e.State,
			"degraded": e.Degraded,
			"tested":   e.Tested,
			"reason":   e.Reason,
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

func (p *Panel) Broadcast(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for ch := range p.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (p *Panel) sub(ch chan string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.subs == nil {
		p.subs = map[chan string]struct{}{}
	}
	p.subs[ch] = struct{}{}
}

func (p *Panel) unsub(ch chan string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.subs, ch)
}

// events is a Server-Sent Events feed for astra-window notifications so an
// open panel tab can pop a browser notification even between polls.
func (p *Panel) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan string, 8)
	p.sub(ch)
	defer p.unsub(ch)
	fmt.Fprintf(w, ":ready\n\n")
	fl.Flush()
	beat := time.NewTicker(25 * time.Second)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			b, _ := json.Marshal(map[string]string{"msg": msg})
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-beat.C:
			fmt.Fprintf(w, ":ping\n\n")
			fl.Flush()
		}
	}
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

func (p *Panel) traceToggle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if p.Trace == nil {
		http.Error(w, "trace not wired", http.StatusServiceUnavailable)
		return
	}
	p.Trace.SetEnabled(body.Enabled)
	writeJSON(w, map[string]any{"trace_enabled": body.Enabled})
}

// traceInterval sets the round interval in seconds (>=60).
func (p *Panel) traceInterval(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seconds int64 `json:"seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if p.Trace == nil {
		http.Error(w, "trace not wired", http.StatusServiceUnavailable)
		return
	}
	p.Trace.SetInterval(time.Duration(body.Seconds) * time.Second)
	writeJSON(w, map[string]any{"trace_interval": p.Trace.IntervalSec()})
}

// traceNow runs one attribution round immediately in the background.
func (p *Panel) traceNow(w http.ResponseWriter, r *http.Request) {
	if p.Trace == nil {
		http.Error(w, "trace not wired", http.StatusServiceUnavailable)
		return
	}
	go p.Trace.Check(context.Background(), p.traceExpected())
	writeJSON(w, map[string]any{"started": true})
}

func (p *Panel) traceExpected() string {
	model := p.TraceModel
	if model == "" {
		model = p.ProbeModel
	}
	if p.Proxy != nil {
		if m := p.Proxy.CanonicalModel(model); m != "" {
			return m
		}
	}
	return model
}

// traceEnabled reports the watchdog switch state.
func (p *Panel) traceEnabled() bool {
	return p.Trace != nil && p.Trace.Enabled()
}

// traceRunning reports whether a round is in flight.
func (p *Panel) traceRunning() bool {
	return p.Trace != nil && p.Trace.Running()
}

func (p *Panel) traceIntervalSec() int64 {
	if p.Trace == nil {
		return 0
	}
	return p.Trace.IntervalSec()
}

func verdictJSON(v mtrace.Verdict) map[string]any {
	return map[string]any{
		"time": v.Time, "expected": v.Expected, "prediction": v.Prediction,
		"family": v.Family, "prob": v.Prob, "match": v.Match,
		"used": v.Used, "err": v.Err,
	}
}

// traceLast returns the most recent verdict for the panel.
func (p *Panel) traceLast() map[string]any {
	if p.Trace == nil {
		return nil
	}
	v, ok := p.Trace.Last()
	if !ok {
		return nil
	}
	return verdictJSON(v)
}

// traceLog returns recent verdicts, newest first.
func (p *Panel) traceLog() []map[string]any {
	if p.Trace == nil {
		return nil
	}
	var out []map[string]any
	for _, v := range p.Trace.Snapshot() {
		out = append(out, verdictJSON(v))
	}
	return out
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
