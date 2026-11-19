# 本地 8000 网关：在 Claude Code / Codex 里切换模型

> 已实测：8000 同时支持 Claude Code 的 `/v1/messages`（Anthropic 协议）和 Codex 的
> `/v1/responses` + `/v1/chat/completions`（OpenAI 协议）。两个客户端都能直连 8000。
> 切换模型 = 改请求里的 `model` 字段名，网关按 `handlers/models.go` 的映射表路由到对应上游。

---

## 0. 前置：需要「网关自己的 key」

8000 要的是**网关 key**（在面板 `/api/keys` 生成的），不是 litellm 的 master key。

⚠️ 关键点：网关 key 的**明文只在「创建那一刻」返回一次**，列表接口返回的是哈希值（不能当 bearer token）。
已为你生成一个可用 key（命名 `cli-8000`，明文如下，请保存，丢失只能重建）：

```
sk-your-gateway-key
```

新建 key 命令（明文只显示一次）：
```bash
TOKEN=$(curl -s -X POST http://localhost:8000/api/auth/login -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin123"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')
curl -s -X POST http://localhost:8000/api/keys -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"key_name":"你的名字"}'
```

---

## 1. Claude Code 配置（指向 8000）

`~/.claude/settings.json`：
```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:8000",
    "ANTHROPIC_API_KEY": "sk-your-gateway-key",
    "ANTHROPIC_MODEL": "glm-4-flash"
  },
  "theme": "dark"
}
```

## 2. Codex 配置（指向 8000）

`~/.codex/config.toml`：
```toml
model_provider = "custom"
model = "glm-4-flash"

[model_providers.custom]
name = "local-8000"
base_url = "http://localhost:8000/v1"
wire_api = "responses"
requires_openai_auth = true
experimental_bearer_token = "sk-your-gateway-key"
```

---

## 3. 怎么切换模型（三种方式）

### A. 临时切换（最推荐，不改配置）
启动客户端时加 `--model`：
```bash
claude  --model deepseek-chat      # 这次会话用 DeepSeek
codex   --model glm-4-flash        # 这次会话用智谱
```
`--model` 会覆盖配置文件里的默认值，关掉就恢复。

### B. 交互式切换（仅 Claude Code REPL）
在 Claude Code 对话里直接输入：
```
/model
```
会弹出模型列表，方向键选完回车即可，当次会话生效。

### C. 永久切换（改配置文件）
- Claude Code：改 `settings.json` 里的 `ANTHROPIC_MODEL`
- Codex：改 `config.toml` 里的 `model = "..."`
改完重启客户端生效。

---

## 4. 8000 上可用的模型名（真实上游）

| 请求 model 名 | 实际上游 | 厂商 |
|---|---|---|
| `glm-4-flash` / `glm-4-flash-fast` / `glm-4-plus` | 智谱 GLM | 清华 Zhipu |
| `deepseek-chat` | DeepSeek-V3 | 深度求索 |
| `qwen-max` / `qwen-plus` / `qwen-turbo` | 通义千问 | 阿里 |
| `kimi` / `moonshot-v1-8k` | Kimi | 月之暗面 |
| `ernie-bot` / `ernie-bot-turbo` | 文心一言 | 百度 |

### ⚠️ 重要坑：`claude-*` / `gpt-*` / `o1/o3/o4-*` 是假别名
8000 网关里这些名字**不会真的调 Anthropic / OpenAI**，而是被映射成 `deepseek-chat`：
- 填 `claude-opus-4-8`、`gpt-5`、`o3-mini` → 实际都跑 **DeepSeek**。
- 想真正用 Claude 类模型，走 190 的 `huoshan-code`（火山方舟 Claude 兼容）；8000 没有配置真实 Claude。
- 所以"切到 glm 而不是 deepseek"时，请**显式写 `glm-4-flash`**，别写 `claude-*`。

---

## 5. 快速验证某模型是否通
```bash
GWKEY=sk-your-gateway-key
# Claude Code 路径
curl -s -X POST http://localhost:8000/v1/messages -H "Authorization: Bearer $GWKEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"glm-4-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'
# Codex 路径
curl -s -X POST http://localhost:8000/v1/responses -H "Authorization: Bearer $GWKEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-chat","input":"hi","max_output_tokens":16}'
```
