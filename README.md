# ccodex-rotate

**本地 Codex 网关：节点轮换转发 + 行为指纹降智检测 + BPS 分流垫片。**

不修改系统代理，不影响你本地的 Clash；macOS 与 Windows 解压即用。

| 它会做 | 它不会做 |
| --- | --- |
| 把 Codex 请求经本地出口原样转发到官方后端 | 不改你的系统代理 / 不动 Clash |
| 出口挂了自动换节点重试 | 不注入、不伪造、不玩凭据窗口 |
| **BPS 垫片按模型分流**：astra / sol 走 Excel 后端，其余回落到 serve 转发 | 日常转发不消耗额外额度 |
| 后台用行为指纹验真身（是否被降智），通知你 | — |

> 上游按（账号，出口，分钟）裁决 serving 模型，且 `served` 字段会撒谎。本工具不追凭据注入，只做**透明转发**，再用**行为归因**给出最终裁决。

---

## 架构总览

项目分两层，代理层必需，垫片层按需：

```
                       ┌─ astra / sol ──▶ bps.openai.com（Excel 后端）
Codex ─▶ bps-shim ─────┤
         (:17852)      └─ 其它模型 ─────▶ ccodex-rotate (:17850) ─▶ 转发出口 (:17890) ─▶ chatgpt.com
                                              │
                                              └─ 降智检测：定期发 3 道随机数挑战做行为指纹归因
```

- **代理层 (ccodex-rotate)**：出口轮换、自动重试、降智检测、图形面板。**必需**。
- **垫片层 (bps-shim)**：把 Codex 请求**按模型分流**，astra / sol 直连 Excel 后端，其余走代理层。**按需**，可完全不用。

不用垫片时，Codex 直连代理层即可：

```
Codex ─▶ ccodex-rotate (:17850) ─▶ 转发出口 (:17890) ─▶ chatgpt.com
```

---

## 30 秒上手（代理层）

```
1. 下载解压   →  2. 双击启动  →  3. 打开面板  →  4. 导入节点  →  5. 重启 Codex，发消息
```

| 步骤 | 平台 | 操作 |
| --- | --- | --- |
| 1. 下载 | 全部 | 到 [Releases](https://github.com/446599/ccodex-rotate/releases) 下载对应压缩包 |
| 2. 启动 | macOS | 双击 `start.command` |
| 2. 启动 | Windows | 双击 `start.cmd` |
| 3. 面板 | 全部 | 自动打开 **http://127.0.0.1:17850/panel**（没节点也能进，先导入） |
| 4. 导入 | 全部 | 面板「订阅管理」粘贴订阅或节点链接（见下） |
| 5. 使用 | 全部 | 重启 Codex 应用 → 新建会话 → 发一条消息 |

各平台文件：

| 平台 | 文件 |
| --- | --- |
| Apple 芯片 Mac | `ccodex-rotate-*-darwin-arm64.zip` |
| Intel Mac | `ccodex-rotate-*-darwin-amd64.zip` |
| Windows x64 | `ccodex-rotate-*-windows-amd64.zip` |
| Windows ARM | `ccodex-rotate-*-windows-arm64.zip` |

> 首次打开面板会自动弹出三步教程，关掉后可在右上角「使用教程」再看。
> 想启用 **BPS 分流**，接着看下一节。

---

## BPS 分流垫片（bps-shim）

仓库 `tools/bps-shim.py` 把 Codex 的 `responses` 请求按模型分流，对客户端表现为一个普通 provider。

### 分流规则

| 模型 | 去向 |
| --- | --- |
| `gpt-6-astra`、`gpt-5.6-sol`（`bps_models`） | 直连 `bps.openai.com`（Excel 后端） |
| 其它模型 | 回落到 `fallback_url`，即代理层 serve 转发 |

### 三步接入

1. **启动垫片**（需 Python 3，代理层建议同时开着）：

   ```sh
   python3 tools/bps-shim.py        # 默认监听 127.0.0.1:17852
   ```

2. **打开垫片面板**：http://127.0.0.1:17852/ ，输入 token（或点设备登录走 OAuth），在面板里存好配置。

3. **切换 Codex 到垫片**：点垫片面板的开关，它会把 Codex 指向 `bps`（先快照原配置，关掉即还原）。也可手填 `~/.codex/config.toml`：

   ```toml
   [model_providers.bps]
   name = "bps_excel"
   base_url = "http://127.0.0.1:17852/v1"
   wire_api = "responses"
   requires_openai_auth = true
   experimental_bearer_token = "BPS_MANAGED"
   ```

   然后确保 `model_provider = "bps"`，重启 Codex 生效。

### 垫片配置

配置文件 `~/.bps-shim/config.json`，也可在垫片面板改：

| 项 | 默认 | 说明 |
| --- | --- | --- |
| `default_model` | `gpt-5.6-sol` | 缺省模型 |
| `default_effort` | 空 | flat `reasoning_effort` |
| `upstream_proxy` | `http://127.0.0.1:17890` | 直连上游时经转发出口 |
| `fallback_url` | `http://127.0.0.1:17850/backend-api/codex/responses` | 非 BPS 模型的回落地址 |
| `bps_models` | `["gpt-6-astra","gpt-5.6-sol"]` | 走 Excel 后端的模型白名单 |

### 行为特性

- 上游静默超 25 秒时发 SSE 心跳保活，长思考轮次不再被掐线。
- 上游无响应头时回诚实 `502`，不制造空 `200` 重连风暴。
- 对 BPS 模型附带画像头与 `model_selection` 等转发约定；fallback 侧纯透传并读取本地账号凭证。

**注意**：脚本内路径（`~/.bps-shim`、`~/.codex`、token 文件）为作者环境硬编码，使用前需自行调整。垫片层与代理层相互独立。

---

## 面板（代理层）

打开 http://127.0.0.1:17850/panel。分页如下：

| 页 | 作用 | 关键按钮 |
| --- | --- | --- |
| 运行概览 | 状态、出口、配额 | **换一个节点**、**恢复自动**、**Astra 提醒** |
| 降智检测 | 指纹检测开关 / 频率 / 记录 | **检测开关**、**保存频率**、**立即检测** |
| 订阅管理 | 增删订阅与节点 | **添加订阅**、**添加节点** |
| 节点列表 | 节点与健康状态 | **用于转发**（固定该节点为出口） |
| 运行日志 | 时间 / 请求模型 / 实际模型 / 状态 / 节点 / 耗时 / 配额 | — |

节点状态徽标：

- `最近模型匹配`：见过它 serving 正确
- `最近模型不匹配`：它 serving 错过（降智）
- `未测`：还没见过
- `已标记可用`：历史请求成功过，**不代表持有凭据**

---

## 导入节点

面板「**订阅管理**」：

- 「订阅链接」框粘贴订阅地址 → **添加订阅**
- 「自定义节点链接」框粘贴分享链接（`ss://` `vmess://` `vless://` `trojan://` `hysteria2://` 等）→ **添加节点**

添加后即时生效，稍等片刻节点列表就会出现。

---

## 降智检测（ModelTrace，可选）

`served` 会撒谎时，用行为归因验真身：发 3 道随机数挑战，按
[ModelTrace](https://github.com/xqy2006/ModelTrace)（MIT，指纹库已内置并署名）的数字指纹归因到 16 个模型；mismatch 即判降智并通知。

- **默认关闭**，每轮消耗 **3 次完整生成**，按需开启。
- 面板可设频率（秒，≥ 60，默认 1800）、立即检测、查看上次结论与记录。
- mismatch 触发桌面 + 浏览器通知（30 分钟冷却）；记录保留最近 50 轮。
- 归因算法与上游 Python 版逐位比对一致（Hellinger + 有序块特征，softmax 校准温度）。

---

## 遇到 502 / 断流怎么办

502 一般是**转发出口这一次没连上**（节点掉线 / 超时）。

1. **偶发 502**：忽略即可，工具会自动换节点重试（默认最多 4 次）。
2. **持续 502**：面板点**换一个节点**；或在节点表对健康节点点**用于转发**；若之前固定过出口，点**恢复自动**。
3. **某订阅大量节点不可用**：到订阅管理清掉坏订阅，只留可达的。
4. **生成中途断流**（回复到一半断开）：工具会标记该节点失败并自动切换，**再发一条**即可。
5. **想更抗超时**：调大配置里的 `timeout_seconds`（默认 120）与 `max_retries`（默认 4）。

---

## 配置文件

| 平台 | 路径 |
| --- | --- |
| macOS | `~/.ccodex-rotate/config.json` |
| Windows | `%USERPROFILE%\.ccodex-rotate\config.json` |

常用项（可复制 `config.example.json` 作模板）：

| 项 | 说明 |
| --- | --- |
| `subscriptions` / `nodes` / `proxies` | 出口来源（订阅 / 分享链接 / 显式代理） |
| `listen` | 本地反向代理地址（默认 `127.0.0.1:17850`） |
| `mixed_port` / `controller_port` | 转发 `17890` / 控制 `17891` |
| `probe_model` | 行为检测默认模型（默认 `gpt-6-astra`） |
| `trace_enabled` / `trace_interval_seconds` / `trace_model` | 指纹检测开关 / 频率秒 / 检测模型（默认关 / `1800` / 跟随 `probe_model`） |
| `notify_enabled` | 降智检出通知（默认 `true`） |
| `panel_password` / `panel_listen` | 面板密码 / 管理端口分离（默认空，单端口无密码） |
| `max_retries` / `timeout_seconds` | 失败重试次数 / 响应头超时秒 |

> 历史版本遗留的采集 / 注入 / 猎手配置项（`inject_state`、`state_lengths`、`collect_*`、`hunt_*`、`cookie_*`、`cred_*`）已失效，会被忽略，可删可不删。

---

## 命令行（可选）

主程序同时提供 CLI，服务与面板也可完全用命令管理：

```sh
ccodex-rotate init                 # 创建默认配置
ccodex-rotate sub add <url...>     # 添加订阅
ccodex-rotate node add <link...>   # 添加节点分享链接
ccodex-rotate serve                # 启动（Ctrl+C 停止并还原 Codex 配置）
ccodex-rotate collect              # 跑一轮行为检测
ccodex-rotate status / nodes       # 查看状态 / 节点
ccodex-rotate fetch-core           # 下载 mihomo 内核（包内已内置，一般不需要）
ccodex-rotate restore              # 还原 Codex 配置
python3 tools/bps-shim.py          # 启动 BPS 分流垫片（默认 127.0.0.1:17852）
```

## 从源码构建（可选）

```sh
./build.sh      # macOS/Linux，产物在 dist/
build.bat       # Windows
```

---

## 说明

- 行为检测每轮消耗 3 次完整生成，按需开启；日常转发不消耗额外额度。
- 上游 verdict 按（账号，出口，分钟）轮转；`served` 不可全信，行为归因是最终裁决。
- 启动窗口 Ctrl+C 退出会自动还原 Codex 配置；不修改系统代理与本地 Clash。
- 本工具与 OpenAI、Mihomo、ModelTrace 无隶属关系；mihomo 内核见 `LICENSE.mihomo`，指纹库来自 ModelTrace（MIT）。
