"use strict";
const $ = (id) => document.getElementById(id);
const esc = (value) =>
  String(value == null ? "" : value).replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const validTime = (value) =>
  value && !value.startsWith("0001") && Number.isFinite(Date.parse(value));
const time = (value) =>
  validTime(value) ? new Date(value).toLocaleString("zh-CN") : "—";
const empty = (title, text) =>
  '<div class="empty"><strong>' +
  esc(title) +
  "</strong><p>" +
  esc(text) +
  "</p></div>";
const table = (headers, rows) =>
  "<table><thead><tr>" +
  headers.map((h) => '<th scope="col">' + esc(h) + "</th>").join("") +
  "</tr></thead><tbody>" +
  rows +
  "</tbody></table>";
function renderHTML(id, html) {
  const element = $(id);
  // Keep keyboard focus and table scroll positions on unchanged poll results.
  if (element.dataset.rendered !== html) {
    element.innerHTML = html;
    element.dataset.rendered = html;
  }
}
let status = null,
  nodes = [],
  refreshing = false,
  mutating = false,
  toastTimer;
async function request(path, body) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 25000);
  try {
    const response = await fetch(path, {
      method: body === undefined ? "GET" : "POST",
      headers: body === undefined ? {} : { "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: controller.signal,
    });
    if (!response.ok)
      throw new Error("请求失败（HTTP " + response.status + "）");
    const result = await response.json();
    if (result.error) throw new Error(result.error);
    return result;
  } finally {
    clearTimeout(timer);
  }
}
function notify(message, failed = false) {
  $("toast").textContent = message;
  $("toast").classList.toggle("error", failed);
  $("toast").hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => ($("toast").hidden = true), 5000);
}
async function mutate(button, path, body = {}, message = "操作已完成") {
  if (mutating) return null;
  mutating = true;
  const label = button.textContent;
  button.disabled = true;
  button.textContent = "处理中…";
  try {
    const result = await request(path, body);
    notify(typeof message === "function" ? message(result) : message);
    await refresh();
    return result;
  } catch (error) {
    notify(
      error.name === "AbortError"
        ? "操作超时，请检查连接后重试。"
        : error.message,
      true,
    );
    return null;
  } finally {
    button.textContent = label;
    button.disabled = false;
    mutating = false;
    if (status) renderControls(status);
  }
}
function renderControls(s) {
  $("mode").textContent = s.manual ? "手动固定" : "自动选择";
  $("rotateBtn").disabled = !!s.manual;
  $("rotateBtn").title = s.manual ? "请先恢复自动，再切换节点" : "切换转发出口";
}
function tick() {
  document.querySelectorAll("[data-exp]").forEach((el) => {
    const seconds = Math.max(
      0,
      Math.ceil((Number(el.dataset.exp) - Date.now()) / 1000),
    );
    el.textContent = seconds
      ? Math.floor(seconds / 60) +
        " 分 " +
        String(seconds % 60).padStart(2, "0") +
        " 秒"
      : "已过期";
  });
}
function renderStatus(s) {
  $("total").textContent = s.total || 0;
  $("requests").textContent = s.requests || 0;
  $("errors").textContent = (s.errors || 0) + " 次错误";
  $("quality").textContent =
    (s.ok || 0) + " 个可用 · " + (s.failed || 0) + " 个失败";
  const last = s.trace_last;
  if (!last) {
    $("traceMetric").textContent = s.trace_enabled ? "等待首轮" : "未开启";
    $("traceMetricSub").textContent = "行为指纹检测";
  } else if (last.err) {
    $("traceMetric").textContent = "检测异常";
    $("traceMetricSub").textContent = "查看检测记录";
  } else if (last.match) {
    $("traceMetric").textContent = last.prediction;
    $("traceMetricSub").textContent =
      "行为一致 · " + Math.round((last.prob || 0) * 100) + "%";
  } else {
    $("traceMetric").textContent = "疑似降智";
    $("traceMetricSub").textContent =
      "预期 " + last.expected + "，实测 " + last.prediction;
  }
  $("node").textContent = s.node || "等待可用节点";
  $("srcCounts").textContent =
    (s.subs || 0) +
    " 订阅 / " +
    (s.nodes || 0) +
    " 节点 / " +
    (s.proxies || 0) +
    " 代理";
  $("setupHint").hidden = !!(s.subs || s.nodes || s.proxies || s.total);
  if (!mutating) renderControls(s);
  renderTrace(s);
  const recent = s.recent || [];
  $("recent").innerHTML = recent.length
    ? table(
        [
          "时间",
          "请求模型",
          "实际模型",
          "状态",
          "转发节点",
          "尝试",
          "耗时",
          "方法 / 路径",
        ],
        recent
          .map(
            (x) =>
              "<tr><td>" +
              esc(new Date(x.time).toLocaleTimeString()) +
              "</td><td>" +
              esc(x.model || "—") +
              "</td><td>" +
              (x.served_model
                ? '<span class="badge ' +
                  (x.model && x.served_model !== x.model ? "bad" : "good") +
                  '">' +
                  esc(x.served_model) +
                  "</span>"
                : "—") +
              '</td><td><span class="badge ' +
              (x.status >= 400 ? "bad" : "good") +
              '">' +
              esc(x.status) +
              "</span></td><td>" +
              esc(x.node) +
              "</td><td>" +
              esc(x.attempts) +
              "</td><td>" +
              esc(x.millis) +
              " ms</td><td>" +
              esc(x.method) +
              " " +
              esc(x.path) +
              "</td></tr>",
          )
          .join(""),
      )
    : empty("暂无会话记录", "在 Codex 中发送消息后，这里会显示转发结果。");
  tick();
}
function renderTrace(s) {
  const on = !!s.trace_enabled;
  $("traceBtn").textContent = on ? "检测：已开启" : "检测：已关闭";
  $("traceBtn").setAttribute("aria-pressed", String(on));
  $("traceBtn").classList.toggle("selected", on);
  $("traceBtn").disabled = false;
  const badge = $("traceBadge");
  const last = s.trace_last;
  if (s.trace_running) {
    badge.textContent = "检测中…";
    badge.className = "badge";
    $("traceLast").textContent = "三道题作答中，请稍候几分钟";
  } else if (!last) {
    badge.textContent = on ? "等待首轮" : "已关闭";
    badge.className = "badge";
    $("traceLast").textContent = "尚无检测";
  } else if (last.err) {
    badge.textContent = "检测异常";
    badge.className = "badge bad";
    $("traceLast").textContent = "异常：" + last.err;
  } else if (last.match) {
    badge.textContent = "正常";
    badge.className = "badge good";
    $("traceLast").textContent =
      time(last.time) +
      " 实测 " +
      last.prediction +
      "（" +
      Math.round((last.prob || 0) * 100) +
      "%，" +
      (last.used || 0) +
      "/3 题有效），与预期一致";
  } else {
    badge.textContent = "疑似降智";
    badge.className = "badge bad";
    $("traceLast").textContent =
      time(last.time) +
      " 预期 " +
      last.expected +
      "，实测 " +
      last.prediction +
      "（" +
      Math.round((last.prob || 0) * 100) +
      "%，" +
      (last.used || 0) +
      "/3 题有效）";
  }
  if (document.activeElement !== $("traceInterval")) {
    $("traceInterval").value = s.trace_interval || 1800;
  }
  const log = s.trace_log || [];
  $("traceLog").innerHTML = log.length
    ? table(
        ["时间", "预期 → 实测", "概率", "有效", "结论"],
        log
          .map(
            (x) =>
              "<tr><td>" +
              esc(time(x.time)) +
              "</td><td>" +
              esc(x.expected || "—") +
              " → " +
              esc(x.prediction || (x.err ? "异常" : "—")) +
              "</td><td>" +
              (x.prob ? Math.round(x.prob * 100) + "%" : "—") +
              "</td><td>" +
              (x.used ? x.used + "/3" : "—") +
              "</td><td>" +
              (x.err
                ? esc(x.err)
                : x.match
                  ? '<span class="badge good">一致</span>'
                  : '<span class="badge bad">降智</span>') +
              "</td></tr>",
          )
          .join(""),
      )
    : empty("暂无检测记录", "打开开关或点「立即检测」跑一轮。");
}
function renderNodes() {
  const query = $("nodeSearch").value.trim().toLowerCase();
  const filtered = nodes.filter((x) =>
    (x.name + " " + (x.type || "")).toLowerCase().includes(query),
  );
  $("nodeCount").textContent = nodes.length ? "· " + nodes.length : "";
  if (!filtered.length) {
    renderHTML(
      "list",
      empty(
        query ? "没有匹配的节点" : "还没有节点",
        query
          ? "试试其他名称或代理类型。"
          : "在上方添加订阅或节点链接，导入后将在这里显示。",
      ),
    );
    return;
  }
  const labels = {
    ok: "已标记可用",
    reachable: "可达 · 未采到",
    unknown: "待检测",
    failed: "暂不可用",
  };
  renderHTML(
    "list",
    table(
      ["节点名称", "类型", "状态", "延迟", "转发操作"],
      filtered
        .map((x) => {
          const pinned = status && status.manual === x.name;
          return (
            '<tr><td title="' +
            esc(x.name) +
            '"><span class="dot ' +
            (Object.hasOwn(labels, x.state) ? x.state : "unknown") +
            '"></span>' +
            esc(x.name) +
            "</td><td>" +
            esc(x.type || "—") +
            "</td><td>" +
            esc(labels[x.state] || x.state) +
            (x.degraded
              ? ' <span class="badge bad" title="最近观测到的响应模型名不匹配">最近模型不匹配</span>'
              : x.tested
                ? ' <span class="badge good" title="最近观测到的响应模型名匹配，不代表已采到有效凭据">最近模型匹配</span>'
                : ' <span class="badge">未测</span>') +
            "</td><td>" +
            (x.alive && x.delay > 0 ? esc(x.delay) + " ms" : "—") +
            '</td><td><button data-pin="' +
            esc(x.name) +
            '" class="' +
            (pinned ? "selected" : "") +
            '" aria-pressed="' +
            !!pinned +
            '">' +
            (pinned ? "已固定转发" : "用于转发") +
            "</button></td></tr>"
          );
        })
        .join(""),
    ),
  );
}
async function refresh() {
  if (refreshing) return;
  refreshing = true;
  try {
    const results = await Promise.all([
      request("/api/status"),
      request("/api/nodes"),
    ]);
    status = results[0];
    nodes = results[1].nodes || [];
    renderStatus(status);
    renderNodes();
    $("connection").textContent = status.mihomo_error
      ? "内核异常"
      : "服务已连接";
    $("connection").className =
      "badge " + (status.mihomo_error ? "bad" : "good");
    $("connectionError").hidden = !status.mihomo_error;
    $("connectionError").textContent = status.mihomo_error
      ? "代理内核异常：" + status.mihomo_error
      : "";
    $("updated").textContent = "更新于 " + new Date().toLocaleTimeString();
  } catch (error) {
    $("connection").textContent = "连接中断";
    $("connection").className = "badge bad";
    $("connectionError").hidden = false;
    $("connectionError").textContent =
      "暂时无法连接本地服务，请确认 ccodex-rotate 正在运行。页面会自动重试；已显示的数据可能过时。";
  } finally {
    refreshing = false;
  }
}
document.querySelectorAll("[data-action]").forEach((button) =>
  button.addEventListener("click", () => {
    const path = button.dataset.action;
    const messages = {
      "/api/rotate": (r) =>
        r.changed ? "已切换转发节点。" : "暂无其他可切换节点。",
      "/api/reset": "已恢复自动选择。",
    };
    mutate(button, path, {}, messages[path]);
  }),
);
$("traceBtn").addEventListener("click", () => {
  if (!status) return;
  mutate(
    $("traceBtn"),
    "/api/trace",
    { enabled: !status.trace_enabled },
    "已更新指纹检测开关。",
  );
});
$("traceSaveBtn").addEventListener("click", () => {
  const v = Math.max(60, parseInt($("traceInterval").value, 10) || 1800);
  mutate($("traceSaveBtn"), "/api/trace-interval", { seconds: v }, "检测频率已保存。");
});
$("traceNowBtn").addEventListener("click", () => {
  mutate($("traceNowBtn"), "/api/trace-now", {}, "已开始一轮检测，请稍候查看结果。");
});
$("list").addEventListener("click", (event) => {
  const button = event.target.closest("[data-pin]");
  if (button)
    mutate(
      button,
      "/api/pin",
      { name: button.dataset.pin },
      "已固定转发出口。",
    );
});
$("nodeSearch").addEventListener("input", renderNodes);
document.querySelectorAll("[data-source]").forEach((form) =>
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const kind = form.dataset.source,
      input = $(kind + "Input"),
      result = $(kind + "Result");
    const lines = input.value
      .split(/\r?\n/)
      .map((x) => x.trim())
      .filter(Boolean);
    if (!lines.length) {
      input.focus();
      return;
    }
    const submitted = input.value;
    const response = await mutate(
      form.querySelector('[type="submit"]'),
      "/api/sources/add",
      { kind, lines },
      (r) =>
        r.added
          ? "已添加 " + r.added + " 项。"
          : "没有新增项目，请检查链接是否有效或已存在。",
    );
    result.classList.toggle("error", !response || !response.added);
    result.textContent = response
      ? "新增 " +
        response.added +
        " 项。" +
        (response.added < lines.length
          ? "重复或无效的链接不会添加，请检查输入。"
          : "正在更新节点列表。")
      : "添加失败，输入已保留，请检查后重试。";
    if (
      response &&
      response.added === lines.length &&
      input.value === submitted
    )
      input.value = "";
  }),
);
document.querySelectorAll("[data-clear]").forEach((button) =>
  button.addEventListener("click", async () => {
    const kind = button.dataset.clear,
      label = kind === "sub" ? "订阅" : "自定义节点";
    if (!confirm("确定清空全部" + label + "？清空后需要重新添加链接。")) return;
    const result = await mutate(
      button,
      "/api/sources/clear",
      { kind },
      "已清空" + label + "。",
    );
    if (result) $(kind + "Result").textContent = "已清空" + label + "。";
  }),
);
// The tutorial is local to this browser; it does not alter proxy configuration.
const guideKey = "ccodex-rotate.guide.v1";
const guideSteps = [
  [
    "添加你的订阅或节点",
    "在「订阅与节点」粘贴订阅地址，点击「添加订阅」。也可以直接添加节点分享链接。",
    "每行一个链接，导入后即时生效。已有节点？直接进入下一步。",
  ],
  [
    "回到 Codex，发一条消息",
    "重启 Codex，新建会话并发送一条消息。工具会获取本次请求的认证信息，用于行为检测。",
    "消息始终正常转发，不受检测开关影响。",
  ],
  [
    "查看状态，开始使用",
    "在概览查看转发出口，在「降智检测」开关行为指纹检测。日常使用保持自动选择即可。",
    "连接不稳定时可「换一个节点」。手动固定只影响消息转发。",
  ],
];
let guideStep = 0,
  returnFocus;
function renderGuide() {
  $("guideNumber").textContent = String(guideStep + 1).padStart(2, "0");
  $("guideTitle").textContent = guideSteps[guideStep][0];
  $("guideText").textContent = guideSteps[guideStep][1];
  $("guideTip").textContent = guideSteps[guideStep][2];
  $("guideSteps").innerHTML = guideSteps
    .map(
      (_, i) =>
        '<span class="' + (i <= guideStep ? "active" : "") + '"></span>',
    )
    .join("");
  $("guideSteps").setAttribute(
    "aria-label",
    "第 " + (guideStep + 1) + " 步，共 3 步",
  );
  $("guideBack").disabled = guideStep === 0;
  $("guideNext").textContent = guideStep === 2 ? "完成，开始使用" : "下一步";
}
function openGuide() {
  returnFocus = document.activeElement;
  guideStep = 0;
  renderGuide();
  $("guide").showModal();
  $("guideNext").focus();
}
function finishGuide() {
  $("guide").close();
}
$("guide").addEventListener("close", () => {
  try {
    localStorage.setItem(guideKey, "seen");
  } catch (_) {}
  if (returnFocus && returnFocus !== document.body) returnFocus.focus();
  else document.querySelector("[data-guide]").focus();
});
$("guideClose").addEventListener("click", finishGuide);
$("guideNext").addEventListener("click", () => {
  if (guideStep === 2) {
    finishGuide();
    return;
  }
  guideStep++;
  renderGuide();
});
$("guideBack").addEventListener("click", () => {
  guideStep = Math.max(0, guideStep - 1);
  renderGuide();
});
document
  .querySelectorAll("[data-guide]")
  .forEach((button) => button.addEventListener("click", openGuide));
const observer = new IntersectionObserver(
  (entries) => {
    entries.forEach((entry) => {
      if (entry.isIntersecting) {
        document.querySelectorAll("nav a").forEach((a) => {
          if (a.hash === "#" + entry.target.id)
            a.setAttribute("aria-current", "location");
          else a.removeAttribute("aria-current");
        });
      }
    });
  },
  { rootMargin: "0px 0px -65% 0px" },
);
document
  .querySelectorAll("main>section")
  .forEach((section) => observer.observe(section));
let seen = false;
try {
  seen = localStorage.getItem(guideKey) === "seen";
} catch (_) {}
if (!seen) openGuide();
setInterval(tick, 1000);
// Astra 到货提醒：SSE 事件 + 浏览器通知 + 声音 + 标题闪烁。
const notifyKey = "ccodex-rotate.notify";
let notifyOn = false,
  titleTimer = null,
  audioCtx = null;
try {
  notifyOn = localStorage.getItem(notifyKey) === "on";
} catch (_) {}
function renderNotifyBtn() {
  const button = $("notifyBtn");
  if (!button) return;
  const supported = "Notification" in window;
  button.textContent =
    "Astra 提醒：" +
    (!supported ? "浏览器不支持" : notifyOn ? "已开启" : "已关闭");
  button.classList.toggle("selected", !!(notifyOn && supported));
  button.disabled = !supported;
}
async function toggleNotify() {
  if (!("Notification" in window)) {
    notify("当前浏览器不支持通知。", true);
    return;
  }
  if (Notification.permission === "granted") {
    notifyOn = !notifyOn;
  } else {
    let permission = "default";
    try {
      permission = await Notification.requestPermission();
    } catch (_) {}
    if (permission !== "granted") {
      notify("浏览器通知未授权，到货提醒无法弹出。", true);
      return;
    }
    notifyOn = true;
  }
  try {
    localStorage.setItem(notifyKey, notifyOn ? "on" : "off");
  } catch (_) {}
  renderNotifyBtn();
  notify(notifyOn ? "Astra 到货提醒已开启。" : "Astra 到货提醒已关闭。");
}
function beep() {
  try {
    const Ctor = window.AudioContext || window.webkitAudioContext;
    audioCtx = audioCtx || new Ctor();
    const osc = audioCtx.createOscillator(),
      gain = audioCtx.createGain();
    osc.connect(gain);
    gain.connect(audioCtx.destination);
    osc.frequency.value = 880;
    gain.gain.value = 0.15;
    osc.start();
    osc.stop(audioCtx.currentTime + 0.4);
  } catch (_) {}
}
function flashTitle(text) {
  const original = "控制台 · ccodex-rotate";
  document.title = text;
  clearTimeout(titleTimer);
  titleTimer = setTimeout(() => {
    document.title = original;
  }, 30000);
}
function connectEvents() {
  let source;
  try {
    source = new EventSource("/api/events");
  } catch (_) {
    return;
  }
  source.onmessage = (event) => {
    let message = "";
    try {
      message = JSON.parse(event.data).msg || event.data;
    } catch (_) {
      message = event.data;
    }
    notify("🟢 " + message);
    flashTitle("🟢 " + message);
    beep();
    if (
      notifyOn &&
      "Notification" in window &&
      Notification.permission === "granted"
    ) {
      try {
        new Notification("ccodex-rotate", { body: message });
      } catch (_) {}
    }
    refresh();
  };
  source.onerror = () => {};
}
$("notifyBtn").addEventListener("click", toggleNotify);
renderNotifyBtn();
connectEvents();
// Single-page navigation: only one section is visible at a time so nodes
// and logs each get their own page. Hash-based: #nodes deep-links.
const PAGES = ["overview", "trace", "sources", "nodes", "activity"];
function showPage(name) {
  if (!PAGES.includes(name)) name = "overview";
  for (const p of PAGES) {
    const el = document.getElementById(p);
    if (el) el.hidden = p !== name;
  }
  document.querySelectorAll("aside nav a").forEach((a) => {
    const target = (a.getAttribute("href") || "").replace(/^#/, "");
    if (!PAGES.includes(target)) return;
    if (target === name) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  });
}
window.addEventListener("hashchange", () =>
  showPage(window.location.hash.replace(/^#/, "")),
);
showPage(window.location.hash.replace(/^#/, ""));
// Schedule after completion so slow requests never overlap.
async function poll() {
  await refresh();
  setTimeout(poll, 3000);
}
poll();
