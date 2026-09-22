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
  $("injBtn").textContent = s.inject ? "凭据注入：已开启" : "凭据注入：已关闭";
  $("injBtn").setAttribute("aria-pressed", String(!!s.inject));
  $("injBtn").classList.toggle("selected", !!s.inject);
  $("injBtn").disabled = false;
  $("mode").textContent = s.manual ? "手动固定" : "自动选择";
  $("rotateBtn").disabled = !!s.manual;
  $("rotateBtn").title = s.manual ? "请先恢复自动，再切换节点" : "切换转发出口";
  $("collectBtn").disabled = !!s.collecting;
  $("stopBtn").hidden = !s.collecting;
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
  const next = $("next");
  if (next.dataset.next) {
    const seconds = Math.max(
      0,
      Math.ceil((Number(next.dataset.next) - Date.now()) / 1000),
    );
    next.textContent = seconds
      ? Math.floor(seconds / 60) +
        " 分 " +
        String(seconds % 60).padStart(2, "0") +
        " 秒后"
      : "等待检查";
  }
}
function renderStatus(s) {
  $("total").textContent = s.total || 0;
  $("requests").textContent = s.requests || 0;
  $("errors").textContent = (s.errors || 0) + " 次错误";
  $("quality").textContent =
    (s.ok || 0) + " 个已采到凭据 · " + (s.failed || 0) + " 个失败";
  $("credentialCount").textContent = (s.states || []).length;
  $("credentialCount").title =
    Number(s.cookie_count) > 0
      ? "Cookies: " + s.cookie_count + " 个，" + s.cookie_age_sec + " 秒前刷新"
      : "Cookies 罐是空的（尚无上游响应）";
  $("node").textContent = s.node || "等待可用节点";
  $("srcCounts").textContent =
    (s.subs || 0) +
    " 订阅 / " +
    (s.nodes || 0) +
    " 节点 / " +
    (s.proxies || 0) +
    " 代理";
  $("setupHint").hidden = !!(s.subs || s.nodes || s.proxies || s.total);
  $("collectBadge").textContent = s.collecting
    ? "正在采集"
    : s.auth_ready
      ? "自动待命"
      : "等待首条消息";
  $("collectBadge").classList.toggle("good", !!s.collecting);
  $("scan").textContent = s.collecting
    ? "正在逐个探测节点，找到合格凭据即停止。"
    : validTime(s.last_collect)
      ? (s.last_collect_ok ? "上次采集成功 · " : "上次未采到 · ") +
        time(s.last_collect)
      : "在 Codex 中发送消息后，自动开始采集。";
  $("progWrap").hidden = !s.collecting;
  const percent = s.collect_total
    ? Math.min(
        100,
        Math.round(((s.collect_tried || 0) * 100) / s.collect_total),
      )
    : 0;
  $("prog").style.width = percent + "%";
  $("prog").parentElement.setAttribute("aria-valuenow", percent);
  $("progText").textContent =
    "已探测 " +
    (s.collect_tried || 0) +
    " / " +
    (s.collect_total || 0) +
    " 个节点";
  delete $("next").dataset.next;
  $("next").textContent = s.collecting ? "采集中" : "等待触发";
  if (!s.collecting && validTime(s.next_collect))
    $("next").dataset.next = Date.parse(s.next_collect);
  if (!mutating) renderControls(s);
  $("cfg").textContent =
    "目标长度 " +
    (s.target_lengths || []).join(" / ") +
    " 字符 · 探测模型 " +
    s.probe_model +
    " · 成功检查间隔 " +
    Math.round(s.success_interval / 60) +
    " 分钟 · 失败重试间隔 " +
    Math.round(s.retry_interval / 60) +
    " 分钟";
  const states = s.states || [];
  $("states").innerHTML = states.length
    ? table(
        ["模型", "长度", "来源节点", "剩余有效期", "已注入", "Cookies"],
        states
          .map(
            (x) =>
              "<tr><td>" +
              esc(x.model) +
              "</td><td>" +
              esc(x.length) +
              " 字符</td><td>" +
              esc(x.node || "—") +
              '</td><td data-exp="' +
              (Date.parse(x.created) + (Number(s.state_ttl) || 3600) * 1000) +
              '" title="采集于 ' +
              esc(time(x.created)) +
              '">—</td><td>' +
              esc(x.hits) +
              " 次</td><td" +
              (Number(x.cookie_count) > 0
                ? ' data-exp="' +
                  (Date.parse(x.created) +
                    (Number(s.cred_ttl) || 240) * 1000) +
                  '" title="240秒凭据窗"'
                : "") +
              ">" +
              (Number(x.cookie_count) > 0
                ? esc(x.cookie_count) + " 个"
                : "—") +
              "</td></tr>",
          )
          .join(""),
      )
    : empty(
        s.collecting ? "正在寻找合格凭据" : "还没有可用凭据",
        !s.auth_ready
          ? "重启 Codex，新建会话发送一条消息，即可触发自动采集。"
          : "正常转发不受影响。可点击「立即采集」重试。" +
              (s.last_seen_len
                ? "最近观测长度：" + s.last_seen_len + " 字符。"
                : ""),
      );
  const recent = s.recent || [];
  $("recent").innerHTML = recent.length
    ? table(
        [
          "时间",
          "请求模型",
          "实际模型",
          "状态",
          "转发节点",
          "注入",
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
              (x.injected ? "已注入" : "未注入") +
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
  const logs = s.collect_log || [];
  $("collectLog").innerHTML = logs.length
    ? table(
        ["时间", "模型", "事件"],
        logs
          .map(
            (x) =>
              "<tr><td>" +
              esc(new Date(x.time).toLocaleTimeString()) +
              "</td><td>" +
              esc(x.model) +
              "</td><td>" +
              esc(x.msg) +
              "</td></tr>",
          )
          .join(""),
      )
    : empty("暂无采集记录", "采集开始后，这里会显示进度和结果。");
  tick();
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
    ok: "已采到凭据",
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
              ? ' <span class="badge bad">降智</span>'
              : x.tested
                ? ' <span class="badge good">不降智</span>'
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
      "/api/collect": "已请求采集，请稍候查看进度。",
      "/api/collect-parallel": (r) =>
        "已开始 " + (r.lanes || "") + " 路并行采集，请稍候查看进度。",
      "/api/collect/stop": (r) =>
        r.stopped ? "本轮采集已停止。" : "当前没有正在进行的采集。",
      "/api/rotate": (r) =>
        r.changed ? "已切换转发节点。" : "暂无其他可切换节点。",
      "/api/reset": "已恢复自动选择。",
    };
    mutate(button, path, {}, messages[path]);
  }),
);
$("injBtn").addEventListener("click", () => {
  if (status)
    mutate(
      $("injBtn"),
      "/api/injection",
      { enabled: !status.inject },
      "已更新凭据注入设置。",
    );
});
$("list").addEventListener("click", (event) => {
  const button = event.target.closest("[data-pin]");
  if (button)
    mutate(
      button,
      "/api/pin",
      { name: button.dataset.pin },
      "已固定转发出口，采集仍自动选路。",
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
    "重启 Codex，新建会话并发送一条消息。工具会获取本次请求的认证信息，并自动开始采集凭据。",
    "采集会消耗少量账号额度。暂时没采到凭据时，消息仍会正常转发。",
  ],
  [
    "查看状态，开始使用",
    "在概览查看转发出口，在「凭据管理」查看有效期与注入次数。日常使用保持自动选择即可。",
    "连接不稳定时可「换一个节点」。手动固定只影响消息转发；采集通道始终独立选路。",
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
// Schedule after completion so slow requests never overlap.
async function poll() {
  await refresh();
  setTimeout(poll, 3000);
}
poll();
