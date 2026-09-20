# ccodex-rotate

本地 Codex 反向代理：自动轮换代理节点、自动获取并注入 turn-state 凭据（个人 292 / Team 332）。
**不修改系统代理，不影响你本地的 Clash。** 支持 macOS 与 Windows，包内已内置 mihomo 内核，解压即用。

---

## 原理（一句话版）

```
Codex  →  ccodex-rotate(本地)  →  代理节点池  →  chatgpt.com
                  │
                  ├─ 转发通道(17890)：你发的消息走这里
                  └─ 采集通道(17892)：后台去找 292/332 凭据走这里
```

- 你发消息时，工具经「**转发出口**」把请求送到 Codex 后端；
- 后台同时用「**采集出口**」逐个节点去请求，拿到 `X-Codex-Turn-State`（个人约 292 字符 / Team 约 332 字符）；
- 采到后缓存，并在后续请求里**自动注入**，让回答更稳定；
- 两个通道**完全独立**，互不影响。

---

## 1. 下载

到 [Releases](https://github.com/446599/ccodex-rotate/releases) 下载对应平台压缩包，解压：

| 平台 | 文件 |
| --- | --- |
| Apple 芯片 Mac | `ccodex-rotate-*-darwin-arm64.zip` |
| Intel Mac | `ccodex-rotate-*-darwin-amd64.zip` |
| Windows x64 | `ccodex-rotate-*-windows-amd64.zip` |
| Windows ARM | `ccodex-rotate-*-windows-arm64.zip` |

## 2. 启动

- **macOS**：双击 `start.command`
- **Windows**：双击 `start.cmd`

启动后自动打开面板：**http://127.0.0.1:17850/panel**（没有节点也能启动，先进面板导入）。

## 3. 导入节点

面板「**订阅与节点**」：

- 在「订阅链接」框粘贴订阅地址 → 点「**添加订阅**」
- 或在「自定义节点链接」框粘贴节点分享链接（`ss://` / `vmess://` / `vless://` / `trojan://` / `hysteria2://` 等）→ 点「**添加节点**」

添加后即时生效，稍等片刻节点列表就会出现。

## 4. Codex 发消息

1. 重启 Codex（ChatGPT 应用），新建会话
2. 发一条消息
3. 之后它会自动采集凭据并注入，你正常用即可

面板「已采集凭据」会显示：模型 / 长度 / 来源节点 / 已注入次数。

---

## 凭据代理 vs 转发代理（重要）

| | 转发代理 | 凭据代理 |
| --- | --- | --- |
| 用途 | 转发你发出去的消息 | 后台采集 292/332 凭据 |
| 通道 | 混合端口 `17890`（组 `CODEX`） | 专用端口 `17892`（组 `COLLECT`） |
| 选路方式 | 默认自动；可**手动固定** | **始终自动轮询**，采到即停 |
| 受手动选择影响 | 会（就是你固定的那个） | **不会** |

也就是说：**你手动固定的出口只用于发消息；采集凭据仍会在所有节点间自动轮询，互不干扰。**

---

## 面板按键功能

概览：
- **换一个节点**：临时换一个转发出口（自动模式下）
- **立即采集 292**：手动触发一次采集（平时发消息会自动触发）
- **恢复自动**：解除手动固定，转发出口回到自动
- **注入凭据：已开启/已关闭**：一键开关是否注入凭据（不影响采集）
- **进度条**：采集时显示「正在逐个节点探测 X / Y（找到即停）」
- **最近观测长度**：显示上游最近返回的凭据长度；若不是 292/332，说明当前出口给的不是目标值

订阅与节点：
- **添加订阅 / 清空订阅**：管理订阅链接
- **添加节点 / 清空节点**：管理自定义节点分享链接

节点表：
- **用于转发**：把该节点固定为转发出口（**只影响发消息**，采集不受影响）
- 状态列 `可用·292` 表示该节点曾产出过合格凭据

最近请求：
- 显示时间 / 方法 / 路径 / 状态 / 节点 / **模型** / **是否注入** / 尝试次数 / 耗时

---

## 遇到 502 / 超时断连怎么办

502 一般是**转发出口这一次没连上**（节点掉线、超时），与凭据无关。按顺序处理：

1. **偶发 502 可忽略**：工具会自动换节点重试（默认最多 4 次），通常你自己不用管。
2. **持续 502**：
   - 面板点「**换一个节点**」，或直接在节点表对一个健康节点点「**用于转发**」；
   - 如果你之前固定过出口，点「**恢复自动**」；
   - 到「最近请求」看是哪个节点、什么状态，避开问题节点。
3. **某个订阅大量节点不可用**（节点质量里“失败”很多）：到「订阅与节点」把坏订阅清掉，只保留可达的。
4. **生成中途断流**（回复到一半断开）：工具会把该节点标记为失败并自动切换，你**再发一条**即可；一般不重复用坏节点。
5. **想更抗超时**：可在配置里调大 `timeout_seconds`（默认 120）与 `max_retries`（默认 4），见下方配置说明。

> 提示：502 与“有没有凭据”无关；没采到凭据时，工具仍会正常转发（只是不注入）。

---

## 配置文件

默认位置：

- macOS：`~/.ccodex-rotate/config.json`
- Windows：`%USERPROFILE%\.ccodex-rotate\config.json`

常用项（可用 `config.example.json` 作模板）：

| 项 | 说明 |
| --- | --- |
| `subscriptions` / `nodes` / `proxies` | 出口来源（订阅 / 节点分享链接 / 显式代理） |
| `listen` | 本地反向代理地址（默认 `127.0.0.1:17850`） |
| `mixed_port` / `collect_port` / `controller_port` | 转发 `17890`、采集 `17892`、控制 `17891` |
| `probe_model` | 采集凭据用的主模型（默认 `gpt-6-astra`） |
| `collect_models` | 额外采集的模型（默认含审查模型 `codex-auto-review`） |
| `state_lengths` | 目标凭据长度：个人 `292`、Team `332`（默认 `[292, 332]`） |
| `auto_collect` | 收到目标模型请求就自动采集（默认 `true`） |
| `inject_state` | 是否注入凭据（默认 `true`，面板也可切换） |
| `max_retries` / `timeout_seconds` | 失败重试次数 / 响应头超时秒数 |
| `collect_success_interval_seconds` / `collect_retry_interval_seconds` | 采到后刷新间隔 / 采不到重试间隔 |

---

## 常用命令（可选）

```sh
ccodex-rotate init                 # 创建默认配置
ccodex-rotate sub add <url...>     # 添加订阅
ccodex-rotate node add <link...>   # 添加节点分享链接
ccodex-rotate serve                # 启动（Ctrl+C 停止并还原 Codex 配置）
ccodex-rotate collect              # 立即采集一次
ccodex-rotate status / nodes       # 查看状态 / 节点
ccodex-rotate fetch-core           # 下载 mihomo 内核（包内已内置，一般不需要）
ccodex-rotate restore              # 还原 Codex 配置
```

---

## 从源码构建（可选）

```sh
./build.sh      # macOS/Linux，产物在 dist/
build.bat       # Windows
```

## 说明

- 采集会消耗少量账号额度；凭据按模型缓存，采到即停，30 分钟后刷新。
- 292（个人）/ 332（Team）为社区经验形状，能否采到取决于账号与出口。
- 退出（启动窗口 Ctrl+C）会自动还原 Codex 配置；不修改系统代理与本地 Clash。
- 本工具与 OpenAI、Mihomo 无隶属关系；包内 mihomo 内核来自 MetaCubeX/mihomo，见 `LICENSE.mihomo`。
