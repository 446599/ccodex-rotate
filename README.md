# ccodex-rotate

本地 Codex 反向代理：节点轮换转发 + 行为指纹降智检测。
**不修改系统代理，不影响你本地的 Clash。** 支持 macOS 与 Windows，包内已内置 mihomo 内核，解压即用。

> 上游按（账号，出口，分钟）裁决 serving 模型（追 292/780 凭据窗口的路子已失效：凭据只活 ~240 秒），served 字段还会撒谎。本工具不玩凭据注入那套：只做透明转发 + 用行为归因验真身。

---

## 原理（一句话版）

```
Codex  →  ccodex-rotate(本地 17850)  →  转发出口(17890/CODEX 组)  →  chatgpt.com
                   │
                   └─ 降智检测：定期发 3 道随机数挑战，做行为指纹归因
```

- 你发消息时，工具经「**转发出口**」原样送到 Codex 后端，**不注入任何东西**；
- 后台按频率跑行为检测：served 字段说 astra，但指纹归因出 luna → 判降智，桌面+面板通知，后台记检测记录。

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

首次在当前浏览器打开面板时，会自动显示三步使用教程。关闭或完成后不再自动弹出，可随时点击右上角「使用教程」重新查看。

面板按运行概览、降智检测、订阅管理、节点列表、运行日志分页，支持节点搜索与手机布局。

## 3. 导入节点

面板「**订阅管理**」：

- 在「订阅链接」框粘贴订阅地址 → 点「**添加订阅**」
- 或在「自定义节点链接」框粘贴节点分享链接（`ss://` / `vmess://` / `vless://` / `trojan://` / `hysteria2://` 等）→ 点「**添加节点**」

添加后即时生效，稍等片刻节点列表就会出现。

## 4. Codex 发消息

1. 重启 Codex（ChatGPT 应用），新建会话
2. 发一条消息
3. 工具顺手记下账号认证，供行为检测复用

---

## 行为指纹检测（ModelTrace 方法，可选）

served 字段会撒谎时，用行为归因验真身：发 3 道随机数挑战，按 ModelTrace（MIT，指纹库已内置署名，见上游
[README](https://github.com/xqy2006/ModelTrace)）的数字指纹归因到 16 个模型，mismatch 即判降智并通知，后台记检测记录。

- 面板「降智检测」页：开关（默认**关闭**，每轮消耗 3 次完整生成）、频率输入（秒，≥60，默认 1800）、立即检测、上次结论、记录表
- mismatch 桌面+浏览器通知（30 分钟冷却）；记录保留最近 50 轮
- 归因算法与上游 Python 版逐位比对一致（Hellinger + 有序块特征，softmax 校准温度）

---

## 面板按键功能

运行概览：
- **换一个节点**：临时换一个转发出口（自动模式下）
- **恢复自动**：解除手动固定，转发出口回到自动
- **Astra 提醒**：浏览器通知授权

降智检测：
- **检测开关**：已开启/已关闭
- **保存频率**：检测间隔秒数
- **立即检测**：马上跑一轮（3 道题，几分钟）

节点列表：
- **用于转发**：把该节点固定为转发出口
- 状态列徽标：`最近模型不匹配`（它 serving 错过）/ `最近模型匹配`（见过它 serving 正确）/ `未测`（还没见过）
- `已标记可用`表示历史请求成功过，不代表持有凭据

运行日志：
- 显示时间 / 请求模型 / 实际模型（ mismatch 标红）/ 状态 / 转发节点 / 尝试次数 / 耗时 / 方法路径 / 周配额

---

## 遇到 502 / 超时断连怎么办

502 一般是**转发出口这一次没连上**（节点掉线、超时）。按顺序处理：

1. **偶发 502 可忽略**：工具会自动换节点重试（默认最多 4 次），通常你自己不用管。
2. **持续 502**：
   - 面板点「**换一个节点**」，或直接在节点表对一个健康节点点「**用于转发**」；
   - 如果你之前固定过出口，点「**恢复自动**」；
   - 到「运行日志」看是哪个节点、什么状态，避开问题节点。
3. **某个订阅大量节点不可用**：到「订阅管理」把坏订阅清掉，只保留可达的。
4. **生成中途断流**（回复到一半断开）：工具会把该节点标记为失败并自动切换，你**再发一条**即可。
5. **想更抗超时**：可在配置里调大 `timeout_seconds`（默认 120）与 `max_retries`（默认 4），见下方配置说明。

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
| `mixed_port` / `controller_port` | 转发 `17890`、控制 `17891` |
| `probe_model` | 行为检测的默认模型（默认 `gpt-6-astra`） |
| `trace_enabled` / `trace_interval_seconds` / `trace_model` | 指纹检测开关/频率秒数/检测模型（默认关/`1800`/跟随 `probe_model`） |
| `notify_enabled` | 降智检出通知（默认 `true`） |
| `panel_password` / `panel_listen` | 面板密码 / 管理端口分离（默认空，即无密码单端口） |
| `max_retries` / `timeout_seconds` | 失败重试次数 / 响应头超时秒数 |

> 历史版本遗留的采集/注入/猎手配置项（如 `inject_state`、`state_lengths`、`collect_*`、`hunt_*`、`cookie_*`、`cred_*`）已失效，留着会被忽略，可删可不删。

---

## 常用命令（可选）

```sh
ccodex-rotate init                 # 创建默认配置
ccodex-rotate sub add <url...>     # 添加订阅
ccodex-rotate node add <link...>   # 添加节点分享链接
ccodex-rotate serve                # 启动（Ctrl+C 停止并还原 Codex 配置）
ccodex-rotate collect              # 跑一轮行为检测
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

- 行为检测每轮消耗 3 次完整生成，按需开启；日常转发不消耗额外额度。
- 上游 verdict 按（账号，出口，分钟）轮转；served 字段不可全信，行为归因是最终裁决。
- 退出（启动窗口 Ctrl+C）会自动还原 Codex 配置；不修改系统代理与本地 Clash。
- 本工具与 OpenAI、Mihomo、ModelTrace 无隶属关系；mihomo 内核见 `LICENSE.mihomo`，指纹库来自 ModelTrace（MIT）。
