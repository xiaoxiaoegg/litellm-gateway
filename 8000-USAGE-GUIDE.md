# 8000 网关：打开面板 / 切换模型 / 对外接入

> 适用：本地 `litellm-gateway`（Docker 栈，运行在你的 Mac 上）
> 服务端口：`8000`（HTTP 明文）与 `8443`（HTTPS，自签名证书）
> 数据：Postgres（`litellm_db` 容器，持久卷，重建不丢）

---

## 1. 面板打不开？先看清两个地址

| 地址 | 协议 | 状态 | 备注 |
|---|---|---|---|
| `http://localhost:8000` | HTTP | ✅ 正常 | **首选**，直接连网关 |
| `https://localhost:8443` | HTTPS | ✅ 正常 | 走 nginx，证书是自签名的，浏览器会报「不安全」→ 点「高级 / 继续访问」即可 |

**最常见的"打不开"原因（都不是服务挂了）：**

1. **用了 `https://localhost:8000`** → 8000 是明文 HTTP，没有 TLS，浏览器直接连不上。
   → 改成 `http://localhost:8000`（不带 s）。
2. **浏览器缓存了旧版白屏页面**（旧版引用过墙 CDN）。
   → 强制刷新一次：`Cmd + Shift + R`（Mac）。之后网关已设 `no-store`，永远拉最新版。
3. **8443 显示「您的连接不是私密连接」** → 这是自签名证书的正常提示，不是故障，点「继续访问」。

验证服务是否真在跑：
```bash
curl -s -o /dev/null -w "8000 HTTP %{http_code}\n" http://localhost:8000/
curl -k -s -o /dev/null -w "8443 HTTP %{http_code}\n" https://localhost:8443/
```
返回 `200` 就说明服务正常，问题在浏览器端。

---

## 2. 怎么换模型（用 glm 而不是 deepseek）

**关键事实：你请求时填的 `model` 名字 = 实际使用的模型。** 网关不会偷偷换（已修复，见下）。

可用模型名（部分）：`deepseek-chat` / `glm-4-flash` / `glm-4-flash-fast` / `qwen-max` / `qwen-plus` / `kimi` / `ernie-bot` / `huoshan-code`（火山 Coding Plan）。
完整列表见 `http://localhost:8000/v1/models`。

### ⚠️ 已修复的坑（2026-07-22）
之前网关的 `ResolveModel` 会把**所有 `glm-` 前缀**兜底改写成 `deepseek-chat`——所以你填 `glm-4-flash` 实际跑的是 DeepSeek。
现已在 `handlers/models.go` 的 `ModelMap` 中显式加回：
```go
"glm-4-flash":      "glm-4-flash",
"glm-4-flash-fast": "glm-4-flash-fast",
"glm-4-plus":       "glm-4-flash",
```
已重新编译容器并实测：`glm-4-flash` → 智谱 GLM；`deepseek-chat` → DeepSeek，互不打扰。

### 各客户端怎么指定模型
- **Claude Code**（`~/.claude/settings.json` 的 `env`）：
  ```json
  { "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:8000",
    "ANTHROPIC_API_KEY": "<网关key>",
    "ANTHROPIC_MODEL": "glm-4-flash"
  }}
  ```
- **Codex**（`~/.codex/config.toml`）：`model = "glm-4-flash"`，`base_url = "http://localhost:8000/v1"`。
- **桌面 GUI（Cherry Studio / Chatbox 等）**：Base URL = `http://localhost:8000/v1`，模型名手动填 `glm-4-flash`。
- **API 直调**：请求体里 `"model": "glm-4-flash"` 即可。

> 注：Claude Code / Codex 默认会发 `claude-*` / `gpt-*` 等名字，网关默认把它们映射到 `deepseek-chat`。
> 想用别的模型，**必须显式把客户端模型名改成上面列出的真实名**（如 `glm-4-flash`）。

---

## 3. 对外接入（让别人 / 你自己远程用）

网关当前只发布在本地（`localhost`）。要对外，按场景选：

### A. 同局域网 / VPN（内网示例：`192.168.1.100:34000`）
1. 查你 Mac 的局域网 IP：
   ```bash
   ipconfig getifaddr en0      # 通常是 192.168.x.x
   ```
2. 同网设备直接访问：
   - `http://192.168.x.x:8000`
   - 或 `https://192.168.x.x:8443`（接受证书警告）
3. Docker 已发布到 `0.0.0.0`，所以同网可达，无需改配置。

### B. 从公网访问（不在同一网络）
**推荐用隧道（不开公网端口、自带 TLS）：**
- **Cloudflare Tunnel**（免费、最稳）：`cloudflared tunnel --url http://localhost:8000`
- **ngrok**：`ngrok http 8000` → 得到公网 `https://xxxx.ngrok.io`
- **Tailscale**（组 VPN）：Mac 和你的设备都装 Tailscale，用 Mac 的 Tailscale IP 访问 `:8000`

**不推荐但可行：** 路由器端口转发，把外网端口 → Mac IP `:8000` / `:8443`。
- ⚠️ 安全：优先转发 `8443`（HTTPS）而非明文 `8000`；面板有登录、API 需 key，但别把明文 `8000` 直接暴露在公网。

### 对外时客户端的 Base URL
把上面本地的 `http://localhost:8000` 换成对外地址即可：
- 同网：`http://192.168.x.x:8000`
- 隧道：`https://你的隧道域名` 或 `https://xxxx.ngrok.io`（都是 HTTPS，端口隐含 443）

---

## 速查命令
```bash
cd ~/Desktop/litellm-gateway
docker compose ps                       # 看容器状态
docker compose build gateway-go && docker compose up -d gateway-go   # 改了 Go 代码后重建
curl -s http://localhost:8000/v1/models | python3 -m json.tool      # 看可用模型
```
