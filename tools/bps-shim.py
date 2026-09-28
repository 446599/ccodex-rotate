#!/usr/bin/env python3
"""bps-shim: Codex -> localhost:17852 -> bps.openai.com Excel backend.
Bridges Codex tool calls through the run_officejs transport convention
(after posystorage/basispoints_plugin_sub4api):
 outbound: drop client tools, prepend developer catalog, wrap continuations
 inbound:  convert run_officejs calls back to real function calls (SSE).
"""
import copy
import json
import subprocess
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LISTEN = ("127.0.0.1", 17852)
UPSTREAM = "https://bps.openai.com/basispoints/api/responses"
TOKEN_FILE = "/Users/henry/bin/.bps-tokens.json"
CLIENT_ID = "app_fnr0pYvVwwFDocDumLG3H2Bp"
TOKEN_URL = "https://auth.openai.com/oauth/token?unified=true"
DEVICE_URL = "https://auth.openai.com/api/accounts/deviceauth"
HOME = "/Users/henry/.bps-shim"
CONFIG_FILE = HOME + "/config.json"
STATE_FILE = HOME + "/state.json"
CODEX_CONFIG = "/Users/henry/.codex/config.toml"
BPS_BLOCK = """
[model_providers.bps]
name = "bps_excel"
base_url = "http://127.0.0.1:17852/v1"
wire_api = "responses"
requires_openai_auth = true
experimental_bearer_token = "BPS_MANAGED"
"""

import os as _os
_os.makedirs(HOME, exist_ok=True)


def load_config():
    cfg = {"default_model": "gpt-5.6-sol", "default_effort": "",
           "upstream_proxy": "http://127.0.0.1:17890",
           "fallback_url": "http://127.0.0.1:17850/backend-api/codex/responses",
           "bps_models": ["gpt-6-astra", "gpt-5.6-sol"]}
    try:
        cfg.update(json.load(open(CONFIG_FILE)))
    except Exception:
        pass
    return cfg


def save_config(cfg):
    try:
        json.dump(cfg, open(CONFIG_FILE, "w"), indent=1)
    except Exception:
        pass


def load_state():
    try:
        return json.load(open(STATE_FILE))
    except Exception:
        return {"enabled": False, "snapshot": ""}


def save_state(st):
    try:
        json.dump(st, open(STATE_FILE, "w"), indent=1)
    except Exception:
        pass

PROFILE_HEADERS = {
    "X-Basispoints-Auth-Mode": "chatgpt",
    "X-OpenAI-Internal-Basispoints-Client-Agent-Profile": "excel",
    "X-OpenAI-Internal-Basispoints-Client-Editor": "excel",
    "X-OpenAI-Internal-Basispoints-Client-Host": "office",
    "X-OpenAI-Internal-Basispoints-Client-Platform": "excel",
    "X-OpenAI-Internal-Basispoints-Client-Platform-Class": "PC",
    "X-OpenAI-Internal-Basispoints-Client-Product": "basispoints-excel-plugin",
    "X-OpenAI-Internal-Basispoints-Client-Runtime": "desktop",
    "X-OpenAI-Internal-Basispoints-Office-Host": "Excel",
    "X-OpenAI-Internal-Basispoints-Office-Platform": "PC",
}

CATALOG_HEAD = (
    "This request is relayed by an external Responses client, not by a live Excel "
    "workbook. The native run_officejs function is a transport endpoint intercepted "
    "by the proxy; do not execute Office code. When a client tool is needed, call "
    "run_officejs exactly once and put a compact JSON object in its code string. "
    "For a function tool use {\"tool\":\"TOOL_NAME\",\"args\":{...}}. "
    "For a custom tool use {\"tool\":\"TOOL_NAME\",\"args\":\"RAW_INPUT\"}. "
    "Never put JavaScript or another run_officejs wrapper inside code. "
    "Do not call any other server-injected Excel/Office/workbook/connector tool. "
    "Use at most one client tool per response. Available client tools:\n"
)
CATALOG_EMPTY = (
    "This request is relayed through the BasisPoints Excel backend by an external "
    "client. Do not call server-injected Excel, Office, workbook, connector, "
    "list_skills, or other native tools. Return assistant text only."
)

def load_tokens():
    try:
        return json.load(open(TOKEN_FILE))
    except Exception:
        return {}


def save_tokens(t):
    try:
        with open(TOKEN_FILE, "w") as f:
            json.dump(t, f)
        import os
        os.chmod(TOKEN_FILE, 0o600)
    except Exception:
        pass


TOK = load_tokens()
TOK_LOCK = threading.Lock()

DEV = {"active": False, "user_code": "", "url": "", "expires": 0,
       "status": "idle", "error": "", "device_auth_id": ""}
DEV_LOCK = threading.Lock()


def valid_token():
    with TOK_LOCK:
        a = TOK.get("access_token", "")
        exp = TOK.get("expires_at", 0)
        if a and exp - time.time() > 120:
            return a
    return refresh_token()


def refresh_token():
    with TOK_LOCK:
        rt = TOK.get("refresh_token", "")
        if not rt:
            return ""
        body = {"grant_type": "refresh_token", "refresh_token": rt,
                "client_id": CLIENT_ID}
    try:
        out = subprocess.run(
            ["curl", "-s", "--noproxy", "*", "--max-time", "30", TOKEN_URL,
             "-H", "Content-Type: application/json",
             "-d", json.dumps(body)],
            capture_output=True, text=True, timeout=40)
        d = json.loads(out.stdout or "{}")
        if not d.get("access_token"):
            return ""
        with TOK_LOCK:
            TOK.update(d)
            TOK["expires_at"] = time.time() + d.get("expires_in", 3600)
            save_tokens(TOK)
            return TOK["access_token"]
    except Exception:
        return ""


def str_val(v):
    return v if isinstance(v, str) else ""


def collect_tools(items):
    """key -> {spec} for function/custom tools, incl. namespace keys and
    nested Lite shapes {"type":"function","function":{...}}."""
    specs = {}

    def walk(value):
        if isinstance(value, dict):
            t = str(value.get("type", "")).lower()
            m = value
            if t == "function" and not value.get("name"):
                fn = value.get("function")
                if isinstance(fn, dict) and fn.get("name"):
                    m = dict(fn)
                    m["type"] = "function"
                    if not m.get("description") and value.get("description"):
                        m["description"] = value["description"]
            name = str_val(m.get("name"))
            ns = str_val(m.get("namespace"))
            if t in ("function", "custom") and name:
                key = ns + "." + name if ns else name
                specs[key] = {"_spec": m, "Name": name, "Namespace": ns,
                              "Type": m.get("type", t)}
            for v in value.values():
                walk(v)
        elif isinstance(value, list):
            for v in value:
                walk(v)

    walk(items)
    return specs


def catalog_lines(specs):
    lines = []
    for key in sorted(specs):
        sp = specs[key]
        base = sp.get("_spec", sp)
        line = "- %s (%s)" % (key, base.get("type", "function"))
        if base.get("description"):
            line += ": " + str(base["description"])[:300].replace("\n", " ")
        if base.get("type") == "function":
            schema = None
            for k in ("parameters", "inputSchema", "input_schema"):
                if isinstance(base.get(k), dict):
                    schema = base[k]
                    break
            if schema is not None:
                try:
                    line += ". JSON Schema: " + json.dumps(schema)[:2000]
                except Exception:
                    pass
        else:
            line += ". Its args value is raw text."
        lines.append(line)
    return lines


def wrap_transport(item):
    """Wrap a client call as run_officejs envelope (full shape)."""
    import time as _t
    call_id = str_val(item.get("call_id"))
    if not call_id:
        call_id = "call_bp_%s" % str_val(item.get("name"))[:8]
    name = str_val(item.get("name"))
    args = item.get("arguments", {})
    if isinstance(args, str):
        try:
            args = json.loads(args)
        except Exception:
            args = {}
    env = {"tool": name, "args": args if isinstance(args, dict) else {}}
    outer = {"summary": "Run client tool " + name, "code": json.dumps(env),
             "destructive": False, "references": []}
    return {"type": "function_call", "id": "fc_" + call_id.replace("call_", "")[:24],
            "call_id": call_id, "name": "run_officejs",
            "arguments": json.dumps(outer), "status": "completed"}


NATIVES = {}
NATIVES_LOCK = threading.Lock()


def translate_input(items, specs):
    out = []
    for raw in items or []:
        if not isinstance(raw, dict):
            continue
        item = dict(raw)
        ty = str(item.get("type", "")).lower()
        if ty == "additional_tools":
            continue
        if ty in ("function_call", "custom_tool_call"):
            call_id = str_val(item.get("call_id"))
            name = str_val(item.get("name"))
            if name in ("run_officejs", "functions.run_officejs"):
                with NATIVES_LOCK:
                    NATIVES[call_id] = dict(item)
                out.append(item)
                continue
            with NATIVES_LOCK:
                if call_id and call_id in NATIVES:
                    out.append(NATIVES[call_id])
                    continue
            if name in specs:
                out.append(wrap_transport(item))
                continue
            out.append(item)
            continue
        if ty in ("function_call_output", "custom_tool_call_output"):
            item["type"] = "function_call_output"
            item.pop("name", None)
            item.pop("namespace", None)
            # Codex mints ctco_* output ids; upstream requires fc* — echo the
            # call's fc-form id (same derivation as convert_call line ~327).
            cid = str_val(item.get("call_id"))
            if cid:
                item["id"] = (cid if cid.startswith("fc")
                              else "fc_" + cid.replace("call_", "")[:24])
            else:
                item.pop("id", None)
            out.append(item)
            continue
        if ty == "reasoning":
            if str_val(item.get("encrypted_content")):
                out.append({"type": "reasoning", "summary": [],
                            "encrypted_content": item["encrypted_content"]})
            continue
        if ty == "item_reference":
            continue
        out.append(item)
    return out


def map_effort(doc, fallback=""):
    eff = ""
    r = doc.get("reasoning")
    if isinstance(r, dict):
        eff = str(r.get("effort", ""))
    eff = (eff or str(doc.get("reasoning_effort", "")) or fallback).strip().lower()
    if eff in ("max", "ultra", "xhigh", "x-high", "extra-high", "extra_high"):
        return "xhigh"
    if eff in ("low", "none", "minimal"):
        return "low"
    if eff == "high":
        return "high"
    return "medium"


TURN = {"task": str(uuid.uuid4()), "turn": None, "iter": 0}
TURN_LOCK = threading.Lock()


def convert_call(native, specs):
    """run_officejs item -> real function_call/custom_tool_call."""
    try:
        args = json.loads(native.get("arguments") or "{}")
        inner = json.loads(args.get("code") or "{}")
    except Exception:
        return None
    name = str_val(inner.get("tool")) or str_val(inner.get("name"))
    if not name or name in ("run_officejs", "functions.run_officejs"):
        return None
    if name == "exec" and "exec_command" in specs:
        name = "exec_command"
    sp = specs.get(name)
    if sp is None:
        return None
    base = sp.get("_spec", sp)
    out_name = sp.get("Name", name)
    ns = sp.get("Namespace", "")
    call_id = str_val(native.get("call_id"))
    if not call_id:
        return None
    item_id = str_val(native.get("id")) or ("fc_" + call_id.replace("call_", "")[:24])
    if base.get("type") == "custom":
        out = {"id": item_id, "type": "custom_tool_call",
               "call_id": call_id,
               "input": inner.get("args") if isinstance(inner.get("args"), str) else ""}
        if ns:
            out["namespace"] = ns
        else:
            out["name"] = out_name
        if out_name == "exec" and not out["input"]:
            # Empty exec hits the harness wrapper's flaky empty path
            # (ReferenceError: console is not defined). Neutral no-op that
            # completes with empty output instead.
            out["input"] = "void 0;"
            trace("EMPTYEXEC %s -> no-op" % call_id)
        return out
        return {"id": item_id, "type": "custom_tool_call", "name": name,
                "call_id": call_id,
                "input": inner.get("args") if isinstance(inner.get("args"), str) else ""}
    a = inner.get("args", inner.get("arguments", {}))
    if not isinstance(a, dict):
        return None
    out = {"id": item_id, "type": "function_call",
           "call_id": call_id, "arguments": json.dumps(a)}
    if ns:
        out["namespace"] = ns
    else:
        out["name"] = out_name
    return out



def convert_native_exec(item, specs):
    """Convert a bps-native exec-style call into the client's exec_command.
    Shape: custom_tool_call name=exec, input=JS containing
    tools.exec_command({...}) or tools.<tool>({...}). Returns item or None."""
    import re
    if item.get("type") not in ("custom_tool_call", "function_call"):
        return None
    name = str_val(item.get("name"))
    if name == "exec" and "exec_command" in specs:
        pass  # fall through to JS parsing below; never pass native exec through
    elif name in specs:
        return item
    blob = str_val(item.get("input")) or str_val(item.get("arguments"))
    if not blob:
        return None
    m = re.search(r'tools\.([A-Za-z_][A-Za-z0-9_]*)\s*\(', blob)
    if not m:
        return None
    target = m.group(1)
    if target not in specs:
        # try unprefixed variants present in catalog
        cands = [k for k in specs if k == target or k.endswith('.' + target)]
        if not cands:
            return None
        target = cands[0]
    # extract balanced {...} after the call paren
    start = blob.index(m.group(0)) + len(m.group(0))
    depth, i, arg = 0, start, ""
    instr, esc = False, False
    for ch in blob[start:]:
        if instr:
            arg += ch
            if esc:
                esc = False
            elif ch == '\\':
                esc = True
            elif ch == '"':
                instr = False
            continue
        if ch == '"':
            instr = True
            arg += ch
        elif ch == '{':
            depth += 1
            arg += ch
        elif ch == '}':
            depth -= 1
            arg += ch
            if depth == 0:
                break
        elif ch == '(':
            depth += 1
            arg += ch
        elif ch == ')':
            if depth == 0:
                break
            depth -= 1
            arg += ch
        else:
            arg += ch
    try:
        args = json.loads(arg)
    except Exception:
        return None
    if not isinstance(args, dict):
        return None
    base = specs[target].get("_spec", specs[target])
    item_id = str_val(item.get("id")) or ("fc_" + str_val(item.get("call_id")).replace("call_", "")[:24])
    out = {"id": item_id, "type": "function_call",
           "call_id": str_val(item.get("call_id")),
           "arguments": json.dumps(args)}
    if specs[target].get("Namespace"):
        out["namespace"] = specs[target]["Namespace"]
    else:
        out["name"] = specs[target].get("Name", target)
    return out


def sse_emit(items, start_idx=0, start_seq=0):
    """Plugin-style synthesis: shelled added, deltas, done, all numbered."""
    lines = []
    seq = start_seq

    def emit(event, payload):
        nonlocal seq
        payload = dict(payload)
        payload["type"] = event
        payload["sequence_number"] = seq
        seq += 1
        lines.append("event: %s\ndata: %s\n\n" % (event, json.dumps(payload)))

    for k, it in enumerate(items):
        i = start_idx + k
        ty = it.get("type")
        if ty == "function_call":
            args = it.get("arguments", "")
            shell = dict(it)
            shell["arguments"] = ""
            shell["status"] = "in_progress"
            emit("response.output_item.added", {"output_index": i, "item": shell})
            if args:
                emit("response.function_call_arguments.delta",
                     {"output_index": i, "item_id": it.get("id"), "delta": args})
            emit("response.function_call_arguments.done",
                 {"output_index": i, "item_id": it.get("id"), "arguments": args})
            emit("response.output_item.done", {"output_index": i, "item": it})
            continue
        if ty == "custom_tool_call":
            data = it.get("input", "")
            shell = dict(it)
            shell["input"] = ""
            emit("response.output_item.added", {"output_index": i, "item": shell})
            if data:
                emit("response.custom_tool_call_input.delta",
                     {"output_index": i, "item_id": it.get("id"), "delta": data})
            emit("response.custom_tool_call_input.done",
                 {"output_index": i, "item_id": it.get("id"), "input": data})
            emit("response.output_item.done", {"output_index": i, "item": it})
            continue
        emit("response.output_item.added", {"output_index": i, "item": it})
        emit("response.output_item.done", {"output_index": i, "item": it})
    return "".join(lines), seq


def codex_status():
    """Read Codex wiring state."""
    try:
        s = open(CODEX_CONFIG).read()
    except Exception as e:
        return {"wired": False, "provider": "?", "model": "?", "error": str(e)[:80]}
    import re
    m = re.search(r'^model_provider\s*=\s*"([^"]+)"', s, re.M)
    mo = re.search(r'^model\s*=\s*"([^"]+)"', s, re.M)
    provider = m.group(1) if m else "?"
    has_block = "[model_providers.bps]" in s
    return {"wired": provider == "bps" and has_block,
            "provider": provider, "model": mo.group(1) if mo else "?"}


def switch_enable():
    """Point Codex at the shim (snapshot current config first)."""
    import re
    try:
        cur = open(CODEX_CONFIG).read()
    except Exception as e:
        return False, str(e)[:120]
    st = load_state()
    if not st.get("snapshot"):
        st["snapshot"] = cur
    if '[model_providers.bps]' not in cur:
        cur = cur.rstrip() + "\n" + BPS_BLOCK
    cur = re.sub(r'^model_provider\s*=.*', 'model_provider = "bps"',
                 cur, flags=re.M)
    try:
        open(CODEX_CONFIG, "w").write(cur)
    except Exception as e:
        return False, str(e)[:120]
    st["enabled"] = True
    save_state(st)
    return True, "ok"


def switch_disable():
    """Restore Codex config to pre-enable snapshot."""
    st = load_state()
    snap = st.get("snapshot", "")
    if not snap:
        # fallback: flip provider back to openai
        try:
            import re
            s = open(CODEX_CONFIG).read()
            s = re.sub(r'^model_provider\s*=.*', 'model_provider = "openai"',
                       s, flags=re.M)
            open(CODEX_CONFIG, "w").write(s)
            st["enabled"] = False
            save_state(st)
            return True, "fallback-openai"
        except Exception as e:
            return False, str(e)[:120]
    try:
        open(CODEX_CONFIG, "w").write(snap)
    except Exception as e:
        return False, str(e)[:120]
    st["enabled"] = False
    save_state(st)
    return True, "ok"


def _direct_post(url, payload, timeout=30):
    import urllib.request
    import urllib.error
    r = urllib.request.Request(url, data=json.dumps(payload).encode(),
                               method="POST",
                               headers={"Content-Type": "application/json"})
    o = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with o.open(r, timeout=timeout) as x:
            return json.load(x)
    except urllib.error.HTTPError as e:
        body = e.read(400).decode("utf-8", "ignore")
        # region-blocked direct egress? retry via the working exit
        if e.code in (401, 403):
            cfg = load_config()
            px = cfg.get("upstream_proxy", "")
            if px:
                import subprocess
                out = subprocess.run(
                    ["curl", "-s", "--max-time", str(timeout), "-x", px, url,
                     "-H", "Content-Type: application/json",
                     "-d", json.dumps(payload)],
                    capture_output=True, text=True, timeout=timeout + 10)
                try:
                    return json.loads(out.stdout or "{}")
                except Exception:
                    pass
        raise RuntimeError("HTTP %s %s" % (e.code, body[:150]))


def device_start():
    with DEV_LOCK:
        if DEV["active"] and time.time() < DEV.get("expires", 0):
            return {"user_code": DEV["user_code"], "url": DEV["url"]}
        try:
            d = _direct_post(DEVICE_URL + "/usercode", {"client_id": CLIENT_ID})
        except Exception as e:
            DEV.update(active=False, status="error", error=str(e)[:150])
            return {"error": str(e)[:150]}
        DEV.update(active=True, user_code=d.get("user_code", ""),
                   url=DEVICE_URL + "/authorize?client_id=" + CLIENT_ID,
                   expires=time.time() + 14 * 60, status="pending", error="",
                   device_auth_id=d.get("device_auth_id", ""))
        th = threading.Thread(target=device_poll, daemon=True)
        th.start()
        return {"user_code": DEV["user_code"], "url": DEV["url"]}


def device_poll():
    deadline = time.time() + 14 * 60
    while time.time() < deadline:
        time.sleep(5)
        with DEV_LOCK:
            if not DEV["active"]:
                return
            payload = {"client_id": CLIENT_ID,
                       "device_auth_id": DEV["device_auth_id"],
                       "user_code": DEV["user_code"]}
        try:
            d = _direct_post(DEVICE_URL + "/token", payload)
        except Exception:
            continue
        if d.get("authorization_code") and d.get("code_verifier"):
            from urllib.parse import urlencode
            body = urlencode(
                {"grant_type": "authorization_code", "client_id": CLIENT_ID,
                 "code": d["authorization_code"],
                 "code_verifier": d["code_verifier"],
                 "redirect_uri": "https://auth.openai.com/basispoints/deviceauth/callback"}).encode()
            import urllib.request
            r = urllib.request.Request(
                TOKEN_URL, data=body, method="POST",
                headers={"Content-Type": "application/x-www-form-urlencoded"})
            try:
                o = urllib.request.build_opener(urllib.request.ProxyHandler({}))
                with o.open(r, timeout=30) as x:
                    t = json.load(x)
                if t.get("access_token"):
                    with TOK_LOCK:
                        TOK.update(t)
                        TOK["expires_at"] = time.time() + t.get("expires_in", 3600)
                        save_tokens(TOK)
                    with DEV_LOCK:
                        DEV.update(active=False, status="done", error="")
                    return
            except Exception as e:
                with DEV_LOCK:
                    DEV.update(active=False, status="error",
                               error="exchange: " + str(e)[:120])
                return
    with DEV_LOCK:
        DEV.update(active=False, status="expired", error="")


def device_status():
    with DEV_LOCK:
        d = dict(DEV)
    d.pop("device_auth_id", None)
    return d


def api_status():
    cfg = load_config()
    st = load_state()
    info = token_info()
    return {"enabled": bool(st.get("enabled")), "codex": codex_status(),
            "config": cfg, "bps": info, "device": device_status()}


def token_info():
    with TOK_LOCK:
        a = bool(TOK.get("access_token"))
        exp = TOK.get("expires_at", 0)
    import base64
    email = ""
    for key in ("id_token", "access_token"):
        try:
            tok = TOK.get(key, "")
            if not tok:
                continue
            seg = tok.split(".")[1]
            seg += "=" * (-len(seg) % 4)
            email = json.loads(base64.urlsafe_b64decode(seg)).get("email", "")
            if email:
                break
        except Exception:
            continue
    return {"logged_in": a, "email": email,
            "expires_in": max(0, int(exp - time.time())) if exp else -1}


def codex_account_auth():
    """Real ChatGPT account auth for the fallback pipe (the shim only ever
    sees BPS_MANAGED from Codex). Read per call so refreshes are picked up."""
    try:
        d = json.load(open(_os.path.expanduser("~/.codex/auth.json")))
        t = d.get("tokens", {}) if isinstance(d, dict) else {}
        return str(t.get("access_token") or ""), str(t.get("account_id") or "")
    except Exception:
        return "", ""


def trace(msg):
    try:
        with open("/tmp/bps-trace.log", "a") as f:
            f.write("%s %s\n" % (time.strftime("%H:%M:%S"), msg))
    except Exception:
        pass


def token_import(body):
    import time as _t
    with TOK_LOCK:
        if body.get("access_token"):
            TOK["access_token"] = body["access_token"]
        if body.get("refresh_token"):
            TOK["refresh_token"] = body["refresh_token"]
        try:
            exp = int(body.get("expires_in", 3600))
        except Exception:
            exp = 3600
        TOK["expires_at"] = _t.time() + exp
        save_tokens(TOK)
    return {"ok": True, "bps": token_info()}


PAGE_HTML = """<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
<title>bps 插件控制台</title>
<style>
:root {
        color-scheme: light;
        --bg: #f5f7f9;
        --surface: #fff;
        --ink: #192a37;
        --muted: #647380;
        --border: #e2e8ed;
        --accent: #146c57;
        --accent-hover: #105341;
        --soft: #eaf5ef;
        --danger: #b53c36;
        --danger-soft: #fbecea;
        --surface-subtle: #f8fafb;
        --surface-hover: #f0f4f5;
        --border-hover: #b9c8ce;
        --input-bg: #fafcfd;
        --input-border: #ccd7df;
        --badge-bg: #eef2f5;
        --badge-ink: #536572;
        --focus: #48a28c;
        --radius: 16px;
        --radius-control: 9px;
        --radius-badge: 6px;
        --font-sans: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif;
        --font-mono: ui-monospace, SFMono-Regular, monospace;
        --motion-fast: 160ms;
        font: 14px/1.6 var(--font-sans);
        color: var(--ink);
        background: var(--bg);
      }
* { box-sizing: border-box; }
body { max-width: 880px; margin: 32px auto; padding: 0 24px 32px; }
h2 { margin: 0 0 24px; font-size: 26px; line-height: 1.4; letter-spacing: -.6px; }
h2 > .muted { display: block; margin-top: 8px; font-weight: 400; }
h3 { display: flex; flex-wrap: wrap; align-items: center; gap: 10px; margin: 0 0 16px; font-size: 16px; }
.card { min-width: 0; background: var(--surface); border: 1px solid var(--border); border-radius: var(--radius); padding: 24px; margin: 18px 0; }
button, input, select { font: inherit; }
button { display: inline-flex; align-items: center; justify-content: center; min-height: 44px; padding: 9px 16px; border-radius: var(--radius-control); border: 1px solid var(--accent); background: var(--accent); color: var(--surface); font-weight: 600; cursor: pointer; transition: background var(--motion-fast), border-color var(--motion-fast), opacity var(--motion-fast); }
button:hover { background: var(--accent-hover); border-color: var(--accent-hover); }
button:active { opacity: .8; }
button.off { background: var(--surface); border-color: var(--border); color: var(--ink); }
button.off:hover { background: var(--surface-hover); border-color: var(--border-hover); }
button.danger { background: var(--danger-soft); border-color: var(--danger); color: var(--danger); }
button:disabled { cursor: not-allowed; opacity: .55; }
input, select { min-width: 0; min-height: 44px; max-width: 100%; padding: 10px 12px; border-radius: var(--radius-control); border: 1px solid var(--input-border); background: var(--input-bg); color: var(--ink); }
input::placeholder { color: var(--muted); opacity: 1; }
:is(button, input, select, a):focus-visible { outline: 3px solid var(--focus); outline-offset: 3px; }
.badge { display: inline-block; padding: 4px 10px; border-radius: var(--radius-badge); font-size: 12px; font-weight: 500; background: var(--badge-bg); color: var(--badge-ink); }
.badge.good { background: var(--soft); color: var(--accent); }
.badge.bad { background: var(--danger-soft); color: var(--danger); }
code { display: block; overflow-wrap: anywhere; background: var(--surface-subtle); border: 1px solid var(--border); padding: 12px; border-radius: var(--radius-control); margin: 10px 0; font: 14px/1.6 var(--font-mono); user-select: all; }
.row { display: flex; gap: 10px; align-items: center; margin: 16px 0; flex-wrap: wrap; }
.row > * { max-width: 100%; }
.muted { color: var(--muted); font-size: 13px; line-height: 1.7; overflow-wrap: anywhere; }
#devBox:not(:empty) { margin: 16px 0; padding: 16px; background: var(--soft); border-radius: var(--radius-control); overflow-wrap: anywhere; }
#swDetail, #tkDetail { min-height: 24px; }
@media (max-width: 600px) {
  body { margin: 20px auto; padding: 0 16px 24px; }
  h2 { font-size: 23px; }
  .card { padding: 18px; }
  .row { gap: 8px; }
  .row input, .row select { width: 100% !important; flex: 1 1 100% !important; font-size: 16px; }
}
@media (prefers-reduced-motion: reduce) {
  *, *::before, *::after { transition: none !important; animation: none !important; }
}
</style></head><body>
<h2>bps 插件控制台 <span class="muted">Codex ↔ Excel 后端</span></h2>
<div class="card"><h3>线路开关 <span id="swBadge" class="badge">…</span></h3>
<div class="muted" id="swDetail">读取中</div>
<div class="row"><button id="swBtn" onclick="toggleSw()">切换</button></div>
<div class="muted">开 = Codex 走垫片（备份原配置）；关 = 恢复备份。CC Switch 可能改写配置，切完看状态。</div></div>
<div class="card"><h3>Excel 会话 <span id="tkBadge" class="badge">…</span></h3>
<div class="muted" id="tkDetail"></div>
<div class="row"><button onclick="devStart()">设备码登录</button></div>
<div id="devBox"></div>
<div class="row"><input id="tkPaste" placeholder="粘贴 access_token（手动导入）" style="flex:1"/>
<button onclick="tkImport()">导入</button></div></div>
<div class="card"><h3>配置</h3>
<div class="row">默认模型 <input id="cfgModel" style="width:10em"/> 强制力度
<select id="cfgEffort"><option value="">跟随请求</option><option>low</option><option>medium</option><option>high</option><option>xhigh</option></select></div>
<div class="row">出口代理 <input id="cfgProxy" style="flex:1" placeholder="http://127.0.0.1:17890（空=直连）"/></div>
<div class="row">模型分流 fallback <input id="cfgFallback" style="flex:1" placeholder="非 bps 模型转发地址"/></div>
<div class="row"><button onclick="cfgSave()">保存配置</button></div></div>
<div class="card"><h3>说明</h3><div class="muted">
请求经 run_officejs 桥转接；turn_id 同轮恒定、agent_iteration 递增。<br/>
配额走 Excel 面（ChatGPT 订阅 agentic 额度），与 Codex 周配额不同池。<br/>
垫片地址 http://127.0.0.1:17852/v1 ，Codex provider 指向它，wire_api=responses。</div></div>
<script>
async function jget(p){const r=await fetch(p);return r.json();}
async function jpost(p,b){const r=await fetch(p,{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(b||{})});return r.json();}
let cur={};
async function refresh(){
  cur=await jget("/api/status");
  const on=!!cur.enabled;
  swBadge.textContent=on?"已开启":"已关闭";swBadge.className="badge "+(on?"good":"");
  swDetail.textContent="Codex provider="+cur.codex.provider+" model="+cur.codex.model+(cur.codex.wired?"（已接垫片）":"");
  swBtn.textContent=on?"关闭":"开启";swBtn.className=on?"danger":"";
  const t=cur.bps;
  tkBadge.textContent=t.logged_in?"已登录":"未登录";tkBadge.className="badge "+(t.logged_in?"good":"bad");
  tkDetail.textContent=t.logged_in?("账号 "+(t.email||"")+"，剩余约"+Math.round(t.expires_in/60)+" 分钟"):"用设备码登录或手动粘贴 token";
  cfgModel.value=cur.config.default_model||"";cfgEffort.value=cur.config.default_effort||"";
  cfgProxy.value=cur.config.upstream_proxy||"";
  cfgFallback.value=cur.config.fallback_url||"";
}
async function toggleSw(){const r=await jpost("/api/switch",{enabled:!cur.enabled});alert(r.ok?(cur.enabled?"已关闭":"已开启"):"失败："+r.msg);refresh();}
async function devStart(){const r=await jpost("/api/device/start",{});if(r.error){alert(r.error);return;}
  devBox.innerHTML="打开 <code>"+r.url+"</code>输入 <code>"+r.user_code+"</code> 登录，完成后自动换 token（轮询中…）";
  const t=setInterval(async()=>{const s=await jget("/api/device/status");
    if(s.status==="done"){clearInterval(t);devBox.innerHTML="登录成功";refresh();}
    else if(s.status==="expired"||s.status==="error"){clearInterval(t);devBox.innerHTML="失败/过期："+(s.error||s.status);}},5000);}
async function tkImport(){const v=tkPaste.value.trim();if(!v)return;
  await jpost("/api/token/import",{access_token:v});tkPaste.value="";refresh();}
async function cfgSave(){await jpost("/api/config",{default_model:cfgModel.value,default_effort:cfgEffort.value,upstream_proxy:cfgProxy.value,fallback_url:cfgFallback.value});alert("已保存");}
refresh();setInterval(refresh,15000);
</script></body></html>"""


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_GET(self):
        from urllib.parse import urlsplit
        path = urlsplit(self.path).path
        if path == "/" or path == "/panel":
            self.serve_page()
            return
        if path == "/api/status":
            self.serve_json(api_status())
            return
        if path == "/api/device/status":
            self.serve_json(device_status())
            return
        if path == "/api/config":
            self.serve_json(load_config())
            return
        if path == "/api/diag":
            import subprocess as _sp, tempfile as _tf4, time as _tt
            cfg = load_config()
            bf = _tf4.NamedTemporaryFile(delete=False, suffix=".json")
            bf.write(b'{"model":"gpt-5.6-sol","input":"hi"}')
            bf.close()
            cmd = ["curl", "-sS", "--no-buffer", "-i", "--max-time", "60"]
            if cfg.get("upstream_proxy"):
                cmd += ["-x", cfg["upstream_proxy"]]
            cmd += ["-X", "POST", UPSTREAM,
                    "-H", "Content-Type: application/json",
                    "--data-binary", "@" + bf.name]
            t0 = _tt.time()
            try:
                pr = _sp.Popen(cmd, stdin=subprocess.DEVNULL,
                               stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE)
                out, err = pr.communicate(timeout=70)
                dt = _tt.time() - t0
                self.serve_json({"rc": pr.returncode, "dt": round(dt, 1),
                                 "out": out[:200].decode("utf-8", "ignore"),
                                 "err": err[:200].decode("utf-8", "ignore")})
            except Exception as e:
                self.serve_json({"excp": str(e)[:200]})
            try:
                import os as _oo
                _oo.unlink(bf.name)
            except Exception:
                pass
            return
        if path.rstrip("/").endswith("/models"):
            try:
                with open(_os.path.expanduser("~/.bps-shim/models.json"), "rb") as _mf:
                    data = _mf.read()
            except Exception:
                data = (b'{"object":"list","data":[{"id":"gpt-5.6-sol","object":"model"},'
                        b'{"id":"gpt-6-astra","object":"model"}],"models":[]}')
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def serve_json(self, obj, code=200):
        data = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def serve_page(self):
        html = PAGE_HTML.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(html)))
        self.end_headers()
        self.wfile.write(html)

    def read_json(self):
        try:
            n = int(self.headers.get("Content-Length", 0))
        except Exception:
            n = 0
        raw = self.rfile.read(n) if n else b"{}"
        try:
            d = json.loads(raw.decode() or "{}")
            return d if isinstance(d, dict) else {}
        except Exception:
            return {}

    def do_POST(self):
        from urllib.parse import urlsplit
        import tempfile as _tf2
        path = urlsplit(self.path).path
        if path == "/api/switch":
            body = self.read_json()
            ok, msg = switch_enable() if body.get("enabled") else switch_disable()
            self.serve_json({"ok": ok, "msg": msg, "status": api_status()})
            return
        if path == "/api/device/start":
            self.serve_json(device_start())
            return
        if path == "/api/token/import":
            body = self.read_json()
            self.serve_json(token_import(body))
            return
        if path == "/api/config":
            body = self.read_json()
            cfg = load_config()
            if "default_model" in body:
                cfg["default_model"] = str(body["default_model"])[:80]
            if "default_effort" in body:
                cfg["default_effort"] = str(body["default_effort"])[:16]
            if "upstream_proxy" in body:
                cfg["upstream_proxy"] = str(body["upstream_proxy"])[:120]
            if "fallback_url" in body:
                cfg["fallback_url"] = str(body["fallback_url"])[:160]
            if "bps_models" in body and isinstance(body["bps_models"], list):
                cfg["bps_models"] = [str(m)[:80] for m in body["bps_models"]][:20]
            save_config(cfg)
            self.serve_json(cfg)
            return
        n = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(n) if n else b"{}"
        inauth = self.headers.get("Authorization") or ""
        try:
            trace("INAUTH %s" % ("none" if not inauth else
                  ("bps-managed" if "BPS_MANAGED" in inauth
                   else "present len=%d" % len(inauth))))
        except Exception:
            pass
        try:
            doc = json.loads(raw.decode() or "{}")
        except Exception:
            doc = {}
        if not isinstance(doc, dict):
            doc = {}
        specs = collect_tools(doc.get("tools"))
        specs.update(collect_tools(doc.get("input")))
        try:
            trace("SPECS %s" % sorted(specs.keys())[:20])
            _ex = specs.get("exec", {})
            _bx = _ex.get("_spec", _ex) if isinstance(_ex, dict) else {}
            trace("EXECSPEC %s" % json.dumps(_bx)[:600])
        except Exception:
            pass
        items = doc.get("input")
        if isinstance(items, str):
            items = [{"type": "message", "role": "user",
                      "content": [{"type": "input_text", "text": items}]}]
        translated = translate_input(items if isinstance(items, list) else [], specs)
        cfg = load_config()
        req_model = str(doc.get("model") or cfg.get("default_model") or "gpt-5.6-sol")
        bps_models = [str(m).lower() for m in
                      (cfg.get("bps_models") or ["gpt-6-astra", "gpt-5.6-sol"])]
        route = "bps" if req_model.lower() in bps_models else "fallback"
        pure = (route == "fallback")
        trace("ROUTE %s -> %s" % (req_model, route))
        if pure:
            # dumb pipe: forward client body verbatim, only force streaming
            turn, it = "fb-" + str(uuid.uuid4()), 0
            body = dict(doc)
            body["model"] = req_model
            body["stream"] = True
            body["store"] = False
            body["input"] = items if isinstance(items, list) else []
        else:
            # continuation? reuse turn, bump iteration
            has_output = any(isinstance(i, dict) and str(i.get("type", "")).lower() == "function_call_output"
                              for i in (translated or []))
            with TURN_LOCK:
                if has_output and TURN["turn"]:
                    turn, it = TURN["turn"], TURN["iter"] + 1
                    TURN["iter"] = it
                else:
                    turn, it = str(uuid.uuid4()), 0
                    TURN["turn"], TURN["iter"] = turn, it
            prologue = []
            instr = doc.get("instructions")
            if isinstance(instr, str) and instr:
                prologue.append({"type": "message", "role": "developer",
                                  "content": [{"type": "input_text", "text": instr}]})
            tc = doc.get("tool_choice")
            if specs and not (isinstance(tc, str) and tc.strip().lower() == "none"):
                prologue.append({"type": "message", "role": "developer",
                                  "content": [{"type": "input_text",
                                               "text": CATALOG_HEAD + "\n".join(catalog_lines(specs))}]})
            elif not specs:
                prologue.append({"type": "message", "role": "developer",
                                  "content": [{"type": "input_text",
                                               "text": CATALOG_EMPTY}]})
            force_eff = (cfg.get("default_effort") or "").strip().lower()
            if force_eff in ("low", "medium", "high", "xhigh"):
                effort = force_eff
            else:
                effort = map_effort(doc, "")
            body = {"model": req_model,
                    "model_selection": "explicit", "stream": True, "store": False,
                    "input": prologue + translated,
                    "reasoning_effort": effort,
                    "metadata": {"task_id": TURN["task"], "turn_id": turn,
                                  "agent_iteration": str(it)}}
        token = valid_token() if not pure else ""
        if not token and not pure:
            err = b'{"error":"bps session expired and refresh failed"}'
            self.send_response(502)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(err)))
            self.end_headers()
            self.wfile.write(err)
            return
        import tempfile as _tfh
        _hf = _tf2.NamedTemporaryFile(delete=False, suffix=".hdr")
        _hdr_path = _hf.name
        _hf.close()
        cmd = ["curl", "-sS", "--no-buffer", "-D", _hdr_path, "--max-time", "280"]
        if pure:
            furl = cfg.get("fallback_url") or "http://127.0.0.1:17850/backend-api/codex/responses"
            trace("FALLBACK %s" % furl)
            cmd += ["-X", "POST", furl,
                   "-H", "Content-Type: application/json",
                   "-H", "Accept: text/event-stream"]
            _ctok, _cacct = codex_account_auth()
            if _ctok:
                cmd += ["-H", "Authorization: Bearer " + _ctok]
            if _cacct:
                cmd += ["-H", "chatgpt-account-id: " + _cacct]
            if not _ctok:
                trace("FALLBACK-AUTH missing-account-token")
            if "127.0.0.1" in furl or "localhost" in furl:
                cmd += ["--noproxy", "*"]
            elif cfg.get("upstream_proxy"):
                cmd += ["-x", cfg["upstream_proxy"]]
        else:
            if cfg.get("upstream_proxy"):
                cmd += ["-x", cfg["upstream_proxy"]]
            else:
                cmd += ["--noproxy", "*"]
            cmd += ["-X", "POST", UPSTREAM,
                   "-H", "Content-Type: application/json",
                   "-H", "Accept: text/event-stream",
                   "-H", "Authorization: Bearer " + token]
            for k, v in PROFILE_HEADERS.items():
                cmd += ["-H", "%s: %s" % (k, v)]
        cmd += ["--data-binary", "@-"]
        trace("CMD %s" % " ".join(
            ("Authorization: Bearer <redacted>" if c.startswith("Authorization:") else c)
            for c in cmd))
        import traceback as _tb
        import tempfile as _tf2
        _bf = _tf2.NamedTemporaryFile(delete=False, suffix=".json")
        _ef = _tf2.NamedTemporaryFile(delete=False, suffix=".err")
        _err_path = _ef.name
        _ef.close()
        try:
            _bf.write(json.dumps(body).encode())
            _bf.close()
            _body_path = _bf.name
        except Exception:
            _body_path = None
        cmd = [c if c != "@-" else ("@" + _body_path if _body_path else "@-") for c in cmd]
        trace("CMD2 %s bodyfile=%s" % (
            " ".join(c for c in cmd if c.startswith("@") or c in ("-X", "POST")),
            _body_path))
        try:
            _eh = open(_err_path, "wb")
            p = subprocess.Popen(cmd, stdin=subprocess.DEVNULL,
                                 stdout=subprocess.PIPE,
                                 stderr=_eh)
        except Exception as e:
            err = json.dumps({"error": str(e)[:150]}).encode()
            self.send_response(502)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(err)))
            self.end_headers()
            self.wfile.write(err)
            return
        code = 200
        # wait for curl's header dump (written as headers arrive), then decide
        t_start = time.time()
        code = 200
        saw_head = False
        _hlines = []
        for _w in range(100):
            time.sleep(0.1)
            try:
                with open(_hdr_path, "rb") as _hf:
                    _hlines = _hf.read().split(b"\n")
                if any(_l.startswith(b"HTTP/") for _l in _hlines):
                    saw_head = True
                    break
            except Exception:
                pass
            if p.poll() is not None:
                break
        for _hl in _hlines:
            _hs = _hl.strip()
            if _hs.startswith(b"HTTP/"):
                saw_head = True
                try:
                    code = int(_hs.split()[1])
                except Exception:
                    pass
        try:
            import os as _osH
            _osH.unlink(_hdr_path)
        except Exception:
            pass
        if not saw_head:
            # curl died before any response headers (DNS/SSL/egress): honest
            # 502 instead of a fake-empty 200 that Codex storms on.
            try:
                _rc0 = p.poll()
            except Exception:
                _rc0 = "?"
            try:
                p.kill()
            except Exception:
                pass
            trace("UP-NOHEAD rc=%s" % _rc0)
            err = json.dumps({"error": "upstream unreachable",
                              "curl_rc": str(_rc0)}).encode()
            self.send_response(502)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(err)))
            self.end_headers()
            try:
                self.wfile.write(err)
            except (BrokenPipeError, ConnectionResetError):
                pass
            return
        if code != 200:
            try:
                _eb = p.stdout.read(65536) or b""
            except Exception:
                _eb = b""
            try:
                p.kill()
            except Exception:
                pass
            trace("UP-ERR %d %s" % (code, _eb[:150]))
            err = json.dumps({"error": "upstream %d" % code,
                              "body": _eb[:500].decode("utf-8", "ignore")}).encode()
            self.send_response(502)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(err)))
            self.end_headers()
            try:
                self.wfile.write(err)
            except (BrokenPipeError, ConnectionResetError):
                pass
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        try:
            self.wfile.flush()
        except Exception:
            pass
        import io as _io
        _cap = _io.BytesIO()
        _out = _io.BytesIO()
        _status_line = ""
        first_at, n_bytes, n_events = 0, 0, 0
        import re
        buf, cur_idx, cur_events = "", None, []
        pending = {}
        passthru = set()
        done_flag = [False]
        emitted = []
        seq = [0]

        def emit_raw(etype, data):
            try:
                _b = (("event: %s\n" % etype).encode()
                      + ("data: %s\n\n" % data).encode())
                self.wfile.write(_b)
                _out.write(_b)
            except (BrokenPipeError, ConnectionResetError):
                pass
            try:
                d = json.loads(data)
                if isinstance(d.get("sequence_number"), int):
                    seq[0] = max(seq[0], d["sequence_number"])
                if etype == "response.output_item.done" and isinstance(d.get("item"), dict):
                    emitted.append(d["item"])
            except Exception:
                pass

        def emit_conv(conv, idx):
            text, seq[0] = sse_emit([conv], idx, seq[0] + 1)
            emitted.append(conv)
            for line in text.splitlines(True):
                try:
                    _b = line.encode()
                    self.wfile.write(_b)
                    _out.write(_b)
                except (BrokenPipeError, ConnectionResetError):
                    pass

        def flush_item(idx):
            if idx is None or idx not in pending:
                return
            item = pending.pop(idx)
            if (isinstance(item, dict) and item.get("type") == "function_call"
                    and item.get("name") in ("run_officejs", "functions.run_officejs")):
                conv = convert_call(item, specs)
                if conv is not None:
                    emit_conv(conv, idx if isinstance(idx, int) else 0)
                    trace("BRIDGE %s -> %s" % (
                        item.get("call_id"), conv.get("name")))
                    return
                trace("BRIDGE-DROP invalid envelope call=%s" % item.get("call_id"))
                return
            if (isinstance(item, dict)
                    and item.get("type") in ("function_call", "custom_tool_call")):
                nm = str_val(item.get("name"))
                if nm in specs:
                    emit_conv(item, idx if isinstance(idx, int) else 0)
                    return
                conv = convert_native_exec(item, specs)
                if conv is not None:
                    emit_conv(conv, idx if isinstance(idx, int) else 0)
                    trace("NATIVE %s -> %s" % (
                        item.get("name"), conv.get("name", conv.get("namespace"))))
                    return
                trace("NATIVE-DROP unknown tool=%s call=%s" % (
                    item.get("name"), item.get("call_id")))
                return
            emit_conv(item, idx if isinstance(idx, int) else 0)

        def handle_event(etype, data):
            nonlocal cur_idx
            try:
                d = json.loads(data) if data not in ("", "[DONE]") else None
            except Exception:
                d = None
            if etype == "response.output_item.added" and isinstance(d, dict):
                cur_idx = d.get("output_index")
                ty = (d.get("item", {}) or {}).get("type", "")
                if ty in ("function_call", "custom_tool_call"):
                    pending[cur_idx] = d.get("item", {})
                else:
                    passthru.add(cur_idx)
                    emit_raw(etype, data)
                return True
            if etype == "response.output_item.done" and isinstance(d, dict):
                idx = d.get("output_index", cur_idx)
                if idx in passthru:
                    passthru.discard(idx)
                    emit_raw(etype, data)
                else:
                    if isinstance(d.get("item"), dict):
                        pending[idx] = d["item"]
                    flush_item(idx)
                cur_idx = None
                return True
            if etype in ("response.function_call_arguments.delta",
                         "response.function_call_arguments.done",
                         "response.custom_tool_call_input.delta",
                         "response.custom_tool_call_input.done",
                         "response.output_text.delta",
                         "response.output_text.done") and isinstance(d, dict):
                idx = d.get("output_index", cur_idx)
                if idx in passthru or idx not in pending:
                    emit_raw(etype, data)
                    return True
                it = pending.get(idx)
                if isinstance(it, dict):
                    if "delta" in d and it.get("type") == "function_call":
                        it["arguments"] = it.get("arguments", "") + str(d["delta"])
                    elif "delta" in d and it.get("type") == "custom_tool_call":
                        it["input"] = it.get("input", "") + str(d["delta"])
                    elif "delta" in d:
                        for c in it.get("content", []) or []:
                            if c.get("type") == "output_text":
                                c["text"] = c.get("text", "") + str(d["delta"])
                    if etype.endswith(".done"):
                        if "arguments" in d and it.get("type") == "function_call":
                            it["arguments"] = d["arguments"]
                        if "input" in d and it.get("type") == "custom_tool_call":
                            it["input"] = d["input"]
                        if "text" in d:
                            for c in it.get("content", []) or []:
                                if c.get("type") == "output_text":
                                    c["text"] = d["text"]
                return True
            if etype in ("response.completed", "response.failed",
                         "response.incomplete") and isinstance(d, dict):
                done_flag[0] = True
                # rewrite output to what the client actually saw (converted
                # items), else Codex sees run_officejs ≠ exec and drops turn.
                try:
                    if isinstance(d.get("response"), dict):
                        d["response"]["output"] = emitted
                    data = json.dumps(d)
                except Exception:
                    pass
                emit_raw(etype, data)
                return True
            return False

        import select as _sel
        _fd = p.stdout.fileno()
        _rbuf = b""
        acc_etype = None
        _alive = [True]

        def _process_line(line):
            nonlocal n_bytes, n_events, first_at, acc_etype
            s = line.strip()
            if not s:
                return
            n_bytes += len(line)
            try:
                _cap.write(line.encode("utf-8", "ignore"))
            except Exception:
                pass
            if s.startswith("event:"):
                n_events += 1
                if not first_at:
                    first_at = time.time()
                    trace("UP-FIRSTBYTE %.1fs" % (first_at - t_start))
                acc_etype = s[6:].strip()
                return
            if s.startswith("data:"):
                d = s[5:].strip()
                if d == "[DONE]":
                    try:
                        self.wfile.write(b"data: [DONE]\n\n")
                        _out.write(b"data: [DONE]\n\n")
                    except (BrokenPipeError, ConnectionResetError):
                        _alive[0] = False
                    return
                if acc_etype and not pure and handle_event(acc_etype, d):
                    acc_etype = None
                    return
                # passthrough unknown events
                try:
                    _b = (("event: %s\n" % acc_etype).encode() if acc_etype else b"")
                    _b += ("data: %s\n\n" % d).encode()
                    self.wfile.write(_b)
                    _out.write(_b)
                except (BrokenPipeError, ConnectionResetError):
                    _alive[0] = False
                acc_etype = None

        while _alive[0]:
            try:
                _r, _, _ = _sel.select([_fd], [], [], 25)
            except Exception:
                break
            if not _r:
                # upstream silent for 25s: SSE comment keepalive. Parsers
                # ignore ':' lines, but the bytes keep idle clients alive.
                if p.poll() is not None:
                    break
                try:
                    self.wfile.write(b":keepalive\n\n")
                    _out.write(b":keepalive\n\n")
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    break
                except Exception:
                    pass
                trace("UP-STALL 25s idle")
                continue
            try:
                _chunk = _os.read(_fd, 65536)
            except Exception:
                break
            if not _chunk:
                break
            _rbuf += _chunk
            while b"\n" in _rbuf:
                _ln, _rbuf = _rbuf.split(b"\n", 1)
                try:
                    _process_line(_ln.decode("utf-8", "ignore"))
                except Exception:
                    pass
                if not _alive[0]:
                    break
        if _rbuf.strip() and _alive[0]:
            try:
                _process_line(_rbuf.decode("utf-8", "ignore"))
            except Exception:
                pass
        try:
            import datetime as _dt
            ts = _dt.datetime.now().strftime("%H%M%S-%f")
            open('/tmp/streams/req-%s-%s.txt' % (ts, body.get("model", "?")[:12]), 'wb').write(_cap.getvalue())
        except Exception:
            pass
        for idx in list(pending):
            flush_item(idx)
        try:
            _rc = p.poll()
        except Exception:
            _rc = "?"
        if _rc == 0 and not done_flag[0] and not pure:
            # clean upstream EOF but no response.completed: synthesize one,
            # else Codex reports "closed before response.completed" and storms.
            seq[0] += 1
            emit_raw("response.completed", json.dumps({
                "sequence_number": seq[0],
                "response": {"id": "resp_" + str(turn).replace("-", "")[:24],
                             "object": "response", "status": "completed",
                             "model": body.get("model", ""), "output": emitted}}))
        try:
            self.wfile.write(b"data: [DONE]\n\n")
            _out.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass
        try:
            import datetime as _dt2
            ts2 = _dt2.datetime.now().strftime("%H%M%S-%f")
            open('/tmp/streams/sent-%s-%s.txt' % (ts2, body.get("model", "?")[:12]), 'wb').write(_out.getvalue())
        except Exception:
            pass
        self.close_connection = True
        trace("UP-DONE %.1fs bytes=%d events=%d rc=%s route=%s" % (
            time.time() - t_start, n_bytes, n_events, _rc, route))
        try:
            _eh.close()
            _ee = open(_err_path, "rb").read()[:200]
            import os as _os4
            _os4.unlink(_err_path)
            if _ee:
                trace("CURLERR %s" % _ee[:150])
        except Exception:
            pass
        try:
            p.kill()
        except Exception:
            pass


if __name__ == "__main__":
    srv = ThreadingHTTPServer(LISTEN, H)
    print("bps-shim on http://%s:%d -> %s" % (LISTEN[0], LISTEN[1], UPSTREAM), flush=True)
    srv.serve_forever()
