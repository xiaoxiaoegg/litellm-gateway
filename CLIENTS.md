# 客户端接入指南（Codex / Claude Code）

本文件记录 AI 编程客户端（**Codex** 与 **Claude Code**）如何接入本项目的 Token 中转站网关。
适用于**重装系统、换机器、客户端版本更新后配置被重置**时，照着配一遍即可恢复。

> 网关地址约定：
> - **本机直连（开发/自测）**：`http://localhost:8000`（gateway-go 自身端口）
> - **对外/同事访问（HTTPS）**：`https://<你的IP或域名>:8443`（经 nginx，已改端口）
> - 管理后台 UI：`https://<IP/域名>:8443/app/`
> - 后台接口：`https://<IP/域名>:8443/api/`

---

## 一、Codex

### 配置文件：`~/.codex/config.toml`

> ⚠️ **重要坑**：`model_provider = "custom"` 必须是**文件最顶层键**（在任何 `[表]` 之前）。
> 若写在某个表头下面（如 `[tui.model_availability_nux]` 之后），Codex 会把它误归入该表并报
> `expected u32, got "custom"` 解析错误。

完整内容（复制即用）：

```toml
model_provider = "custom"

[features]
collaboration_modes = true

[projects."~/your-project"]
trust_level = "trusted"

[tui.model_availability_nux]
"gpt-5.6-sol" = 4

[model_providers.custom]
name = "gateway"
base_url = "http://127.0.0.1:8000/v1"
wire_api = "responses"
requires_openai_auth = true
experimental_bearer_token = "sk-your-gateway-key"
```

### 选择模型

Codex 走 `/v1/responses` 协议，模型名直接填网关暴露的名字：

- 火山方舟（Coding Plan）：`ark-code-latest`
- DeepSeek：`deepseek-chat` / `deepseek-v4-pro` / `gpt-5.6-sol`

### 非交互测试命令

```bash
codex exec "用一句话中文介绍你自己" -m ark-code-latest
# 若报错 "not a git repo / untrusted"，加参数：
codex exec "用一句话中文介绍你自己" -m ark-code-latest --skip-git-repo-check
```

### CLI 安装位置（本机）

Codex 通过 npm 安装，二进制在 `~/.npm-global/bin/codex`。
若终端 `which codex` 找不到，确保 PATH 包含该目录：
```bash
export PATH="$HOME/.npm-global/bin:$PATH"
```

---

## 二、Claude Code

### 配置文件：`~/.claude/settings.json`

> ⚠️ **最关键坑**：`settings.json` 里的 `env` 字段，**优先级高于 shell 的环境变量**。
> 如果这里写死了 `ANTHROPIC_BASE_URL`，你在终端 `export` 的网关地址会被**完全无视**。
> 之前这里曾写死直连火山方舟（`https://ark.cn-beijing.volces.com/api/coding`），
> 导致 Claude Code 绕过网关、直连方舟且用错 key → 401。现在已改为走网关。

完整内容（复制即用）：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:8000",
    "ANTHROPIC_API_KEY": "sk-your-gateway-key",
    "ANTHROPIC_MODEL": "huoshan-code"
  },
  "theme": "dark"
}
```

### 选择模型 —— 必须用 `huoshan-code` 别名

> ⚠️ **重要坑**：不要填 `ark-code-latest`！
> Claude Code 看到 `ark-` 前缀会把模型**识别为火山方舟原生模型**，从而**绕过 `ANTHROPIC_BASE_URL` 直连方舟**，
> 导致不走网关、鉴权失败。
> 网关侧已为方舟注册中性别名 **`huoshan-code`**（不带 `ark-` 前缀），用这个别名 Claude Code 才会乖乖走网关。

- 火山方舟（走网关）：`huoshan-code`
- DeepSeek：`deepseek-chat`（同样走网关）

> 注：网关 `/v1/messages` 用 `huoshan-code` / `deepseek-chat` 均已实测通过（返回中文正常）。

### 非交互测试命令

```bash
claude -p "用一句话中文介绍你自己"
# -p = print 模式（非交互，自动退出，适合联调测试）
# 模型已默认设为 huoshan-code，无需额外 -m 参数
```

### CLI 安装位置（本机）

Claude Code 通过 npm 安装，二进制在 `~/.npm-global/bin/claude`。
若 `which claude` 找不到，确保 PATH 包含 `~/.npm-global/bin`。

---

## 三、对外给同事 / 其他设备

| 客户端 | base_url | 鉴权 | 模型名 |
|---|---|---|---|
| Codex | `https://<IP/域名>:8443/v1` | `Authorization: Bearer <网关Key>` | `ark-code-latest` |
| Claude Code | `https://<IP/域名>:8443`（设 `ANTHROPIC_BASE_URL`） | `ANTHROPIC_API_KEY=<网关Key>` | `huoshan-code` |

- 同事需要先向你在网关后台（`/api/admin/users` 或管理 UI）**建账号、发 Key**，用自己的 Key 调，独立计费。
- 对外访问需 HTTPS（Claude Code / Codex 对非 localhost 的 http 会拒绝）。证书为 `localhost` 自签，浏览器/客户端会报证书警告属正常；正式对外用见 `DEPLOY.md` 换真域名证书。

---

## 四、排错速查

| 现象 | 原因 | 解决 |
|---|---|---|
| Codex 启动报 `expected u32, got "custom"` | `model_provider` 没写在文件最顶层 | 把 `model_provider = "custom"` 移到 config.toml 第 1 行 |
| Codex 报 untrusted / not a git repo | 当前目录未信任 | 命令加 `--skip-git-repo-check`；或加 `[projects."<路径>"] trust_level="trusted"` |
| Claude Code 卡死无输出 / 401 | `settings.json` 的 env 覆盖了网关地址，直连方舟 | 改 `settings.json` env 为 `localhost:8000`；删掉方舟 key |
| Claude Code 走方舟而非网关 | 模型名用了 `ark-code-latest`（`ark-` 前缀触发原生路由） | 改用别名 `huoshan-code` |
| 网关侧某模型报 `model not found` | litellm 配置未加载该模型 | 改 `litellm-config.yaml` 后必须 `docker compose up -d --force-recreate litellm` 重读 `.env` 新变量 |

---

## 五、安全提醒

- ⚠️ 火山方舟的 Key（`ark-9d6...`）是**他人分享的 Coding Plan**，每次调用消耗的是**对方的套餐额度**，非本人。
- 网关 master key（`sk-cd8...`）默认弱口令，对外部署前务必在 `.env` 改成强随机值（见 `DEPLOY.md`）。
- 不要开着 CC Switch 这类会改写客户端配置的工具，它会把 base_url 改回直连真实上游。
