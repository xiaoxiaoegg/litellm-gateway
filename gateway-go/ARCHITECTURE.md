# 网关架构速查（gateway-go）

> 这是一套用 Go (gin) 写的 LLM 中转网关：对外暴露 OpenAI / Anthropic / Responses 三种协议端点，
> 统一鉴权、计费、限额后转发到上游（LiteLLM / DeepSeek / 千帆 / Kimi 等）。
> 本文档只讲「代码怎么组织、请求怎么流、改代码看哪里」，细节以源码注释为准。

---

## 1. 一句话总览

```
客户端(Claude Code / Codex / 任意 OpenAI SDK)
        │  三种协议
        ▼
[gin 网关 :8000]  鉴权(APIKey) → 计费/限额 → 协议转换 → 转发
        │
        ▼
上游(LiteLLM :4000 / DeepSeek / 千帆 / Kimi ...)
```

- **管理面**（注册/登录/用户/定价/统计）用 **JWT** 鉴权。
- **代理面**（真正打模型的端点）用 **API Key**（`sk-...`）鉴权。
- 三种客户端协议最终都被翻译成 **OpenAI chat/completions** 再发上游。

---

## 2. 目录与文件职责

| 文件 | 职责 | 改代码时的关键点 |
|------|------|------------------|
| `main.go` | 服务入口、路由四大分组、中间件挂载、优雅关闭 | 加新端点在这里挂路由 |
| `middleware/auth.go` | JWT 鉴权 / AdminOnly / APIKeyAuth（x-api-key + Bearer 双兼容）、master key 直通、`GenerateToken` | 调整鉴权规则 |
| `models/models.go` | 6 张表结构（用户 / API Key / 上游 Key / 定价 / 调用日志 等） | 加字段先改这里再迁移 |
| `database/db.go` | 连接串、`AutoMigrate` 自动建表 | 换数据库 |
| `database/seed.go` | admin 账号 + 默认定价种子（幂等） | 改初始数据 |
| `handlers/models.go` | `ModelMap` 模型映射 + 前缀兜底 | **加新模型/供应商第一站** |
| `handlers/chat.go` | OpenAI 代理主流程、上游 Key 回退、千帆优先、每日配额、乐观锁扣费 | **改计费/限额看这里** |
| `handlers/anthropic.go` | Anthropic ↔ OpenAI 双向转换 + 流式 SSE（Claude Code 走这里） | 加新客户端协议参照它 |
| `handlers/responses.go` | Responses API ↔ OpenAI 转换（Codex 实际走这里），工具名非空过滤 | Codex 相关问题看这里 |
| `handlers/metrics.go` | Prometheus 指标（客户端 / 模型 / 状态码 三维） | 加监控维度 |
| `handlers/auth.go` | 注册 / 登录（发 JWT） | — |
| `handlers/keys.go` | API Key 的增删查（存哈希不存明文） | — |
| `handlers/qianfan.go` | 千帆 OAuth 直连 | — |
| `handlers/stats.go` | 统计聚合（调用量 / 花费） | — |
| `handlers/admin.go` | 后台：用户 / 上游 Key / 定价管理 | — |

---

## 3. 三条协议链路（重点）

### 3.1 OpenAI 原生 —— `POST /v1/chat/completions`
最直接：客户端本来就是 OpenAI 格式，鉴权+计费后几乎透传上游。
`handlers/chat.go` 是所有链路的**计费与扣费核心**，另外两条最终也复用它的下游逻辑。

### 3.2 Anthropic（Claude Code）—— `POST /v1/messages`
Claude Code 用 Anthropic SDK，请求体是 Anthropic 格式，需要双向翻译：

```
Anthropic 请求  ──convert──▶  OpenAI 请求 ──▶ 上游
Anthropic 响应  ◀──convert──  OpenAI 响应
```

- 关键：`content` 可能是**数组**（文本块 + tool_use 块），要拆开重组。
- 流式：把 OpenAI 的 `delta` SSE 翻译成 Anthropic 的 `content_block_delta` / `tool_use` 事件。
- 见 `handlers/anthropic.go`。

### 3.3 Responses API（Codex）—— `POST /v1/responses`
Codex（v0.144+）用的是 Responses API，不是 chat/completions：

```
Responses 请求(input[]) ──convert──▶ OpenAI messages[] ──▶ 上游
Responses 响应          ◀──convert──  OpenAI 响应
```

- 关键坑：工具数组里 `function.name` 为空会被上游拒（400），转换时**过滤掉空 name**。
- `input` 数组要拉平成 `messages`；流式事件要按 Responses 的 `response.output_text.delta` 等格式拼装。
- 见 `handlers/responses.go`。

---

## 4. 鉴权与计费流程

```
请求进来
  │
  ├─ APIKeyAuth: 取 x-api-key 或 Authorization: Bearer
  │     ├─ 是 master key? → 直通（不计费）
  │     └─ 否 → 查库(比对哈希) → 取到用户 & 余额
  │
  ├─ 每日配额: 按天滚动重置，超了直接 429
  │
  ├─ 转发上游拿到 usage(token 数)
  │
  └─ 计费: cost = prompt*price_in + completion*price_out
        └─ 乐观锁扣费: UPDATE balance = balance - cost WHERE balance >= cost
             (防并发扣穿)
```

- 定价表按 `model_name` 匹配；找不到用默认价。
- 上游 Key 也按 `model_name` 匹配选择，支持多供应商并存。

---

## 5. 常见改动速查

| 我想… | 改哪里 |
|-------|--------|
| 加一个新模型别名 | `handlers/models.go` 的 `ModelMap` |
| 接一个新上游供应商 | `models.go` 映射 + `chat.go` 的 `defaultBaseURL` / 上游 Key 表 |
| 调整计费公式 | `chat.go` 的 `calculateCost` / `finishChat` |
| 改每日限额逻辑 | `chat.go` 的配额段 |
| 支持一种新客户端协议 | 照 `anthropic.go` / `responses.go` 新增一个转换 handler，再在 `main.go` 挂路由 |
| 加监控指标维度 | `handlers/metrics.go` |
| 改鉴权规则 | `middleware/auth.go` |

---

## 6. 部署提醒

- 目录里的 `gateway` 二进制是历史编译产物，**改完源码要重新 `docker compose build`** 才生效。
- 交叉编译目标：`linux/arm64`。
- Prometheus 抓 `gateway-go:8000/metrics`；Grafana 已配「客户端调用量」面板。
- 指标是**内存计数器**，容器重建会清零。

---

*本速查随代码演进，请在大改后同步更新。详细逐行说明见各 `.go` 文件顶部与关键函数处的中文注释。*
