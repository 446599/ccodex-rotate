// Package web serves a minimal status/control panel for ccodex-rotate.
package web

import (
	"context"
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
	mux.HandleFunc("/panel/", p.page)
	mux.HandleFunc("/panel", p.page)
	mux.HandleFunc("/api/status", p.status)
	mux.HandleFunc("/api/nodes", p.nodes)
	mux.HandleFunc("/api/rotate", p.rotate)
	mux.HandleFunc("/api/collect", p.collect)
	mux.HandleFunc("/api/collect/stop", p.collectStop)
	mux.HandleFunc("/api/scan", p.collect)
	mux.HandleFunc("/api/injection", p.injection)
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

const pageHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>ccodex-rotate</title>
<style>
:root{color-scheme:light dark}
body{font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:0 auto;padding:20px;max-width:1080px}
h1{font-size:18px;margin:0 0 4px}
h2{font-size:14px;margin:0 0 8px}
.muted{opacity:.65}
.card{border:1px solid #8883;border-radius:10px;padding:14px 16px;margin:12px 0}
.row{display:flex;justify-content:space-between;gap:12px;flex-wrap:wrap}
button{font:inherit;padding:6px 12px;border-radius:8px;border:1px solid #8886;background:#8881;cursor:pointer}
button:hover{background:#8882}
table{width:100%;border-collapse:collapse;font-size:13px}
td,th{padding:5px 8px;border-bottom:1px solid #8882;text-align:left;white-space:nowrap}
.dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:6px}
.ok{background:#2ecc71}.warn{background:#f1c40f}.bad{background:#e74c3c}.unk{background:#95a5a6}
code{background:#8882;padding:1px 5px;border-radius:5px}
.tag{display:inline-block;padding:1px 7px;border-radius:20px;background:#8882;font-size:12px}
.cols{display:flex;gap:12px;flex-wrap:wrap;align-items:flex-start}
.cols>.card{flex:1 1 360px;min-width:300px}
</style></head>
<body>
<h1>ccodex-rotate</h1>
<p class="muted">本地 Codex 反向代理 · 逐节点采集 292 凭据并同节点注入 · 故障自动轮换</p>

<div class="card">
  <h2>概览</h2>
  <div class="row"><b>转发出口</b><span id="node" class="muted">…</span></div>
  <div class="row"><span>节点质量（可用292 / 可达 / 未测 / 失败 / 总）</span><span id="counts2" class="muted">…</span></div>
  <div class="row"><span>请求 / 错误</span><span id="counts" class="muted">…</span></div>
  <div class="row"><span>上次采集</span><span id="scan" class="muted">…</span></div>
  <div id="progWrap" style="display:none;margin:4px 0">
    <div style="background:#8882;border-radius:6px;height:10px;overflow:hidden">
      <div id="prog" style="background:#2ecc71;height:10px;width:0%"></div>
    </div>
    <div id="progText" class="muted" style="font-size:12px">…</div>
  </div>
  <div class="row"><span>下次采集</span><span id="next" class="muted">…</span></div>
  <div class="row"><span>注入凭据</span><button id="injBtn" onclick="toggleInject()">…</button></div>
  <div class="row"><span>目标长度 / 探测模型</span><span id="cfg" class="muted">…</span></div>
  <div class="row"><span>代理地址 / 上游</span><span class="muted"><code>{{.Listen}}</code> → <code>{{.Upstream}}</code></span></div>
  <div class="row" style="margin-top:10px">
    <span><button onclick="act('/api/collect')">立即采集 292</button>
    <button id="stopBtn" onclick="act('/api/collect/stop')" style="display:none;border-color:#e74c3c">停止采集</button>
    <button onclick="act('/api/rotate')">换一个节点</button>
    <button onclick="act('/api/reset')">恢复自动</button></span>
  </div>
</div>

<div class="card">
  <h2>订阅与节点（自定义链接）</h2>
  <p class="muted">每行一个。订阅支持 Clash / Base64 / URI 列表；节点支持 ss / vmess / vless / trojan / hysteria2 / http / socks5。保存后即时生效，不影响本地 Clash。</p>
  <div class="row"><span>当前：</span><span id="srcCounts" class="muted">…</span></div>
  <div style="margin-top:8px"><b>订阅链接</b></div>
  <textarea id="subInput" rows="2" style="width:100%;box-sizing:border-box" placeholder="https://你的订阅链接"></textarea>
  <div style="margin-top:6px"><button onclick="addSrc('sub')">添加订阅</button>
  <button onclick="clearSrc('sub')">清空订阅</button></div>
  <div style="margin-top:10px"><b>自定义节点链接</b></div>
  <textarea id="nodeInput" rows="2" style="width:100%;box-sizing:border-box" placeholder="ss://... 或 vmess://... 每行一个"></textarea>
  <div style="margin-top:6px"><button onclick="addSrc('node')">添加节点</button>
  <button onclick="clearSrc('node')">清空节点</button></div>
  <p id="srcResult" class="muted"></p>
</div>

<div class="card">
  <h2>已采集凭据 (X-Codex-Turn-State)</h2>
  <div id="states" class="muted">—</div>
</div>

<div class="card">
  <h2>节点</h2>
  <p class="muted">「使用」= 固定转发出口（仅影响消息转发；采集凭据仍自动轮询，不受影响）。「恢复自动」解除固定。</p>
  <div id="list" class="muted">加载中…</div>
</div>

<div class="cols">
  <div class="card">
    <h2>凭据获取日志</h2>
    <div id="collectLog" class="muted">—</div>
  </div>
  <div class="card">
    <h2>会话日志</h2>
    <div id="recent" class="muted">—</div>
  </div>
</div>
<script>
async function act(p){await fetch(p,{method:'POST'});refresh();}
async function addSrc(kind){
  var el=document.getElementById(kind==='sub'?'subInput':'nodeInput');
  var lines=el.value.split(/\r?\n/).map(function(s){return s.trim();}).filter(Boolean);
  if(!lines.length)return;
  var r=await (await fetch('/api/sources/add',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({kind:kind,lines:lines})})).json();
  document.getElementById('srcResult').textContent=(r.error?('出错：'+r.error):('已添加 '+r.added+' 项'));
  el.value='';refresh();
}
async function clearSrc(kind){
  await fetch('/api/sources/clear',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({kind:kind})});
  document.getElementById('srcResult').textContent='已清空';refresh();
}
async function toggleInject(){
  var on=!window._inj;
  await fetch('/api/injection',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({enabled:on})});
  refresh();
}
async function use(name){await fetch('/api/pin',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name})});refresh();}
function esc(s){return String(s==null?'':s).replace(/[&<>"']/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c];});}
function ts(v){if(!v||v.indexOf('0001')===0)return '—';return new Date(v).toLocaleString();}
function ago(v){if(!v||v.indexOf('0001')===0)return '';var s=Math.floor((Date.now()-new Date(v))/1000);if(s<60)return s+'秒前';if(s<3600)return Math.floor(s/60)+'分前';return Math.floor(s/3600)+'时前';}
function fmtLeft(ms){if(isNaN(ms))return '—';if(ms<=0)return '已过期';var s=Math.floor(ms/1000);var h=Math.floor(s/3600);var m=Math.floor((s%3600)/60);var sec=s%60;function p(n){return (n<10?'0':'')+n;}return (h>0?(h+':'):'')+p(m)+':'+p(sec);}
function tick(){var now=Date.now();document.querySelectorAll('[data-exp]').forEach(function(el){el.textContent=fmtLeft(Number(el.dataset.exp)-now);});document.querySelectorAll('[data-next]').forEach(function(el){el.textContent=fmtLeft(Number(el.dataset.next)-now);});}
setInterval(tick,1000);
var stateLabel={ok:'可用',reachable:'无法获取',unknown:'未测',failed:'失败'};
var stateDot={ok:'ok',reachable:'warn',unknown:'unk',failed:'bad'};
async function refresh(){
 try{
  var s=await (await fetch('/api/status')).json();
  document.getElementById('node').textContent=(s.node||'(自动)')+(s.manual?('  【手动·仅转发】'):'  【自动】');
  document.getElementById('counts2').textContent=s.ok+' / '+s.reachable+' / '+s.unknown+' / '+s.failed+' / '+s.total;
  document.getElementById('counts').textContent=s.requests+' / '+s.errors;
  var sc='—';
  if(s.collecting)sc='采集中…';
  else if(s.last_collect&&s.last_collect.indexOf('0001')!==0)
    sc=ts(s.last_collect)+' · '+(s.last_collect_ok?'成功（已注入）':'未采到')+' '+ago(s.last_collect);
  document.getElementById('scan').textContent=sc;
  var nx=document.getElementById('next');
  if(s.collecting){nx.textContent='采集中…';nx.removeAttribute('data-next');}
  else if(s.next_collect&&s.next_collect.indexOf('0001')!==0){nx.setAttribute('data-next',new Date(s.next_collect).getTime());}
  else{nx.textContent='—';nx.removeAttribute('data-next');}
  window._inj=!!s.inject;
  var ib=document.getElementById('injBtn');
  if(ib){ib.textContent=s.inject?'已开启（点击关闭）':'已关闭（点击开启）';ib.style.borderColor=s.inject?'#2ecc71':'#e74c3c';}
  var pw=document.getElementById('progWrap');
  if(pw){
    if(s.collecting){pw.style.display='block';var tot=s.collect_total||0;var tr=s.collect_tried||0;var pct=tot?Math.floor(tr*100/tot):0;document.getElementById('prog').style.width=pct+'%';document.getElementById('progText').textContent='正在逐个节点探测 '+tr+' / '+tot+'（找到即停）';}
    else{pw.style.display='none';}
  }
  var sb=document.getElementById('stopBtn');
  if(sb) sb.style.display=s.collecting?'inline-block':'none';
  document.getElementById('cfg').textContent=(s.target_lengths||[]).join(',')+' 字符 · '+s.probe_model+' · 成功'+s.success_interval+'s/失败'+s.retry_interval+'s';
  if(document.getElementById('srcCounts')) document.getElementById('srcCounts').textContent='订阅 '+s.subs+' · 节点 '+s.nodes+' · 代理 '+s.proxies;
  var st=s.states||[];
  var stMsg;
  var obs = s.last_seen_len ? ('最近观测到 '+esc(s.last_seen_model||'')+' 返回 '+s.last_seen_len+' 字符（非目标）') : '';
  if(s.collecting) stMsg='正在采集…'+(s.last_seen_len?('（已观测 '+s.last_seen_len+' 字符）'):'');
  else if(!s.auth_ready) stMsg='尚未获得账号凭据：在 Codex 里发一条消息后会自动开始采集';
  else stMsg='尚无合格 292/332 凭据，正在按间隔重试'+(obs?('；'+obs):'');
  document.getElementById('states').innerHTML = st.length
    ? '<table><tr><th>模型</th><th>长度</th><th>来源节点</th><th>类型</th><th>采集时间</th><th>剩余有效期</th><th>已注入</th></tr>'+
      st.map(function(x){var exp=new Date(x.created).getTime()+(Number(s.state_ttl)||3600)*1000;var src=(x.source==='traffic')?'会话':'探测';return '<tr><td>'+esc(x.model)+'</td><td><b>'+x.length+'</b> 字符</td><td>'+esc(x.node||'-')+'</td><td>'+src+'</td><td>'+ts(x.created)+' '+ago(x.created)+'</td><td data-exp="'+exp+'">—</td><td>'+x.hits+'</td></tr>';}).join('')+'</table>'
    : stMsg;
  tick();
  var n=await (await fetch('/api/nodes')).json();
  var rows=(n.nodes||[]).map(function(x){
    var lb=stateLabel[x.state]||x.state;
    var pinned=(s.manual&&x.name===s.manual);
    var btn='<button onclick="use(\''+esc(x.name)+'\')"'+(pinned?' style="border-color:#2ecc71;color:#2ecc71;font-weight:bold"':'')+'>'+(pinned?'已固定转发':'用于转发')+'</button>';
    return '<tr><td><span class="dot '+(stateDot[x.state]||'unk')+'"></span>'+esc(x.name)+'</td><td>'+esc(x.type||'')+'</td><td>'+lb+'</td><td>'+(x.alive?(x.delay>0?x.delay+' ms':'—'):'—')+'</td><td>'+btn+'</td></tr>';
  }).join('');
  document.getElementById('list').innerHTML='<table><tr><th>节点</th><th>类型</th><th>状态</th><th>延迟</th><th></th></tr>'+rows+'</table>';
  var rr=(s.recent||[]).map(function(x){
    return '<tr><td>'+new Date(x.time).toLocaleTimeString()+'</td><td>'+esc(x.method)+'</td><td>'+esc(x.path)+'</td><td>'+x.status+'</td><td>'+esc(x.node)+'</td><td>'+esc(x.model||'')+'</td><td>'+(x.injected?'是':'否')+'</td><td>'+x.attempts+'</td><td>'+x.millis+' ms</td></tr>';
  }).join('');
  var mh=(s.recent||[]).map(function(x){return x.model;}).filter(Boolean)[0]||'模型';
  document.getElementById('recent').innerHTML = rr
    ? '<table><tr><th>时间</th><th>方法</th><th>路径</th><th>状态</th><th>节点</th><th>'+esc(mh)+'</th><th>注入</th><th>尝试</th><th>耗时</th></tr>'+rr+'</table>'
    : '—';
  var cl=(s.collect_log||[]).map(function(x){
    return '<tr><td>'+new Date(x.time).toLocaleTimeString()+'</td><td>'+esc(x.model||'')+'</td><td>'+esc(x.msg)+'</td></tr>';
  }).join('');
  document.getElementById('collectLog').innerHTML = cl
    ? '<table><tr><th>时间</th><th>模型</th><th>事件</th></tr>'+cl+'</table>'
    : '—';
  if(s.mihomo_error){var el=document.getElementById('recent');el.innerHTML='<b style="color:#e74c3c">'+esc(s.mihomo_error)+'</b><br>'+el.innerHTML;}
 }catch(e){document.getElementById('list').textContent='读取失败：'+e;}
}
setInterval(refresh,3000);refresh();
</script>
</body></html>`
