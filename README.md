# ccodex-rotate

本地 Codex 反向代理：自动轮换代理节点、自动获取并注入 turn-state 凭据（个人 292 / Team 332）。
**不修改系统代理，不影响你本地的 Clash。**

支持 macOS 与 Windows，包内已内置 mihomo 内核，解压即用。

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

启动后会弹出网页面板：**http://127.0.0.1:17850/panel**

> 首次没有节点也能启动，面板会引导你导入。

## 3. 导入节点

在面板「**订阅与节点**」里：

- 「订阅链接」框粘贴订阅地址 → 点「**添加订阅**」
- 或在「自定义节点链接」框粘贴节点分享链接（`ss://` / `vmess://` / `vless://` / `trojan://` / `hysteria2://` 等）→ 点「**添加节点**」

添加后即时生效，稍等片刻「节点」列表就会出现，面板会显示可用节点数量。

## 4. Codex 发消息

1. 重启 Codex（ChatGPT 应用），新建会话
2. 随便发一条消息
3. 之后它会**自动采集 292 / 332 凭据并注入**，你正常用即可

面板「已采集凭据」里能看到：模型、长度、来源节点、已注入次数。

---

## 常用操作（面板）

- **换一个节点**：临时换转发出口
- **用于转发**：固定某个节点作为转发出口（只影响消息转发；采集凭据仍自动轮询，不受影响）
- **恢复自动**：解除固定，回到自动
- **立即采集 292**：手动触发一次采集

## 常见问题

- **面板打不开**：确认启动窗口还开着；地址是 http://127.0.0.1:17850/panel
- **提示找不到内核**：发布包已内置 `mihomo`/`mihomo.exe`；若自行构建，运行 `ccodex-rotate fetch-core` 或 `ccodex-rotate core <路径>`
- **一直没采到凭据**：当前出口可能给的是非目标长度，它会每 5 分钟自动重试；也可在面板换节点后再点「立即采集 292」
- **想退出**：在启动窗口按 Ctrl+C，会自动还原 Codex 配置

---

## 从源码构建（可选）

```sh
./build.sh      # macOS/Linux，产物在 dist/
build.bat       # Windows
```

## 说明

- 采集会消耗少量账号额度；凭据按模型缓存，采到即停，30 分钟后刷新。
- 292（个人）/ 332（Team）为社区经验形状，能否采到取决于账号与出口。
- 本工具与 OpenAI、Mihomo 无隶属关系；包内 mihomo 内核来自 MetaCubeX/mihomo，见 `LICENSE.mihomo`。
