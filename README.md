# ccodex-rotate

本地 Codex 反向代理：**代理节点池轮换 + 惰性健康切换 + 按模型采集/注入 292 turn-state**。

它在本机启动一个私有 mihomo 内核和一个反向代理，把 Codex 的请求经健康节点转发到 `chatgpt.com`，并按需采集 `X-Codex-Turn-State`（个人账号通常 **292** 字符）再注入后续请求。全程**不修改系统代理、不影响你本地的 Clash**。

支持 macOS 与 Windows。

---

## 功能

- **节点轮换 / 故障切换**：订阅或自定义节点组成出口池，失败的节点进入冷却、自动跳过。
- **按需采集 292**：只在目标模型缺 292 时采集；**逐个节点串行探测、采到即停**，成功 30 分钟后再刷新，失败 5 分钟后再试。
- **同节点 / 跨节点注入**：采到的 292 绑定来源节点；默认允许跨节点注入。
- **模型名归一**：`gpt-5.4-high`、`gpt-5.4` 视为同一模型，避免采错/注错。
- **面板 + 命令行**：可视化节点质量、凭据、最近请求；支持订阅与自定义节点链接。
- **安全**：不触碰系统代理与本地 Clash；`serve` 退出时自动还原 Codex 配置。

---

## 下载与安装

到 [Releases](https://github.com/446599/ccodex-rotate/releases) 下载对应平台压缩包：

| 平台 | 文件 |
| --- | --- |
| Apple 芯片 Mac | `ccodex-rotate-*-darwin-arm64.zip` |
| Intel Mac | `ccodex-rotate-*-darwin-amd64.zip` |
| Windows x64 | `ccodex-rotate-*-windows-amd64.zip` |
| Windows ARM | `ccodex-rotate-*-windows-arm64.zip` |

解压后目录包含：可执行文件、启动脚本、`config.example.json`。

### 前置条件

需要一个可用的 **mihomo / Clash.Meta 内核**（本工具不内置）。常见来源：

- 已安装 Clash Verge / Mihomo Party（会自动探测其内核路径）
- 或自行下载 mihomo，在配置里用 `mihomo_path` 指定

---

## 快速开始

### macOS

1. 解压后双击 `start.command`（或终端 `./start.command`）。
2. 首次会提示配置订阅：

   ```sh
   ./ccodex-rotate sub add "https://你的订阅链接"
   ```

3. 重新运行 `./start.command`，看到 `codex wired ...` 即接入成功。
4. 重启 Codex，新建会话，发一条消息。

### Windows

1. 解压后双击 `start.cmd`。
2. 首次提示配置订阅：

   ```bat
   ccodex-rotate.exe sub add "https://你的订阅链接"
   ```

3. 重新双击 `start.cmd`。
4. 重启 Codex，新建会话，发一条消息。

### 网页面板

启动后打开：**http://127.0.0.1:17850/panel**

- 概览：当前出口、节点质量、请求/错误、上次/下次采集
- **订阅与节点**：粘贴订阅链接或节点分享链接（`ss://`、`vmess://`、`vless://`、`trojan://`、`hysteria2://`、`http(s)://`、`socks5://`），保存即时生效
- 已采集凭据：模型 / 长度 / 来源节点 / 已注入次数
- 节点表、最近请求
- 按钮：**立即采集 292**、换一个节点、恢复自动

---

## 命令行

```sh
ccodex-rotate init                     # 创建默认配置
ccodex-rotate sub add <url...>         # 添加订阅
ccodex-rotate sub list / rm / clear    # 查看/删除/清空订阅
ccodex-rotate node add <link...>       # 添加自定义节点分享链接
ccodex-rotate node list / clear
ccodex-rotate proxy add <uri...>       # 添加 http/https/socks5 代理
ccodex-rotate serve                    # 启动（代理 + 面板），Ctrl+C 停止并还原
ccodex-rotate collect                  # 立即采集一次（逐个节点，采到即停）
ccodex-rotate status                   # 查看运行状态
ccodex-rotate nodes                    # 查看节点与健康
ccodex-rotate check                    # 校验配置
ccodex-rotate restore                  # 还原 Codex 配置
ccodex-rotate paths                    # 显示配置/数据/Codex 路径
```

常用参数：`--config PATH`、`--codex-home PATH`。

---

## 配置

配置文件默认在：

- macOS：`~/.ccodex-rotate/config.json`
- Windows：`%USERPROFILE%\.ccodex-rotate\config.json`

可用 `config.example.json` 作模板。关键项：

| 项 | 说明 |
| --- | --- |
| `subscriptions` / `nodes` / `proxies` | 出口来源（订阅 / 节点分享链接 / 显式代理） |
| `listen` | 本地反向代理地址（默认 `127.0.0.1:17850`） |
| `mixed_port` / `controller_port` | 私有 mihomo 端口（默认 `17890` / `17891`） |
| `mihomo_path` | 手动指定 mihomo 内核路径（留空自动探测） |
| `probe_model` | 采集 292 用的模型（默认 `gpt-6-astra`） |
| `state_lengths` | 目标凭据长度（默认 `[292]`；也可加 `312`） |
| `auto_collect` | 是否在收到目标模型请求时自动采集（默认 `false`，手动触发） |
| `collect_success_interval_seconds` | 采到后多久再刷新（默认 1800） |
| `collect_retry_interval_seconds` | 采不到多久重试（默认 300） |
| `model_aliases` | 自定义模型名归一映射 |

---

## 从源码构建

需要 Go 1.26+。

```sh
./build.sh          # macOS/Linux：输出到 dist/
build.bat           # Windows：双击运行（调用 build.ps1）
```

会生成 `darwin-arm64`、`darwin-amd64`、`windows-amd64`、`windows-arm64` 四个平台二进制。

运行测试：

```sh
go test ./...
```

---

## 说明与边界

- 不修改系统代理、DNS、TUN，也不停止任何本地程序；使用独立的私有 mihomo 与端口。
- `serve` 会把 `~/.codex/config.toml` 指向本地代理，退出时自动还原（备份为 `config.toml.ccodex-rotate.bak`）。
- 采集会消耗少量账号额度；`state_lengths` 只接受目标长度，非目标长度不会被缓存或注入。
- 292 属于社区经验形状，不是 OpenAI 官方指标；能否采到取决于账号与出口。
