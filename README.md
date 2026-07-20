# Token 中转站

基于 LiteLLM 的企业级 AI 网关管理平台。参考 One API / New API 的设计模式，提供用户管理、API Key 鉴权、积分计费、多上游 Key 故障切换等能力。

## 技术栈

| 层 | 技术 |
|---|---|
| **模型路由** | LiteLLM |
| **管理 API** | FastAPI + SQLAlchemy |
| **数据库** | PostgreSQL 16 |
| **前端** | HTML + Alpine.js + Tailwind CSS |
| **监控** | Prometheus + Grafana |
| **代理** | Nginx |
| **容器化** | Docker Compose |

## 架构

```
用户 ─→ Web UI (Alpine.js)
         │
         ▼
    管理 API (FastAPI) ───── PostgreSQL
         │                       │
         ├── LiteLLM ───── 智谱 AI (glm-4-flash)
         │
         └── 上游 Key 池 ── 自动故障切换
                             配额管理
```

## 快速启动

```bash
# 1. 配置环境变量
cp .env.example .env  # 编辑 .env 填入你的 API Key

# 2. 启动所有服务
docker compose up -d

# 3. 访问管理界面
open http://localhost:8000/login.html
```

默认管理员账号：`admin` / `admin123`

## API 文档

### 认证

所有 API 端点（除 login/register 外）都需要在 HTTP Header 中传递 JWT Token：
```
Authorization: Bearer <token>
```

### 用户端 API

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/auth/register` | 注册 |
| POST | `/api/auth/login` | 登录，返回 JWT |
| GET | `/api/auth/me` | 当前用户信息 |
| GET | `/api/keys` | 我的 API Key 列表 |
| POST | `/api/keys` | 创建 API Key |
| DELETE | `/api/keys/{id}` | 删除 API Key |
| GET | `/api/balance` | 我的积分余额 |
| GET | `/api/transactions` | 交易记录 |
| GET | `/api/usage` | 使用记录 |

### 管理端 API（需要 admin 角色）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/admin/users` | 用户列表 |
| POST | `/api/admin/users` | 创建用户 |
| GET | `/api/admin/users/{id}` | 用户详情 |
| POST | `/api/admin/users/{id}/recharge` | 充值积分 |
| POST | `/api/admin/users/{id}/deduct` | 扣减积分 |
| GET | `/api/admin/upstream-keys` | 上游 Key 列表 |
| POST | `/api/admin/upstream-keys` | 添加上游 Key |
| PUT | `/api/admin/upstream-keys/{id}` | 更新上游 Key |
| DELETE | `/api/admin/upstream-keys/{id}` | 删除上游 Key |
| GET | `/api/admin/pricing` | 模型定价列表 |
| POST | `/api/admin/pricing` | 设置定价 |
| PUT | `/api/admin/pricing/{id}` | 更新定价 |

### OpenAI 兼容的聊天 API

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/chat/completions` | 聊天完成（需传 API Key 鉴权） |

此端点完全兼容 OpenAI 格式，可用任何 OpenAI SDK 调用。鉴权方式为在 HTTP Header 中传递用户管理的 API Key（非 JWT）：

```bash
curl http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer sk-xxxxxxxx..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-4-flash",
    "messages": [{"role": "user", "content": "你好"}]
  }'
```

请求成功后会根据 token 消耗自动扣减用户积分。

## 数据库模型

```
users                 用户账号、积分、角色
user_api_keys         用户管理的 API Key（用于 OpenAI 兼容接口）
upstream_keys         上游模型提供商的多 Key 池（故障切换用）
model_pricing         各模型积分定价表
usage_records         详细使用记录（token 数、消耗）
transactions          资金变更流水（充值/消费）
```

## 积分体系

- 1 元人民币 = 10000 积分
- 积分按模型定价自动扣减
- 默认模型 `glm-4-flash` 定价：
  - Prompt: 0.15 积分 / 1K tokens
  - Completion: 0.60 积分 / 1K tokens

## 上游 Key 故障切换

同一个模型可以配置多个上游 API Key。系统按优先级排序，当第一个 Key 配额耗尽或请求失败时，自动切换到下一个可用 Key。

## 监控

- Prometheus: `http://localhost:9090`
- Grafana: `http://localhost:3000` (admin / admin123)
  - LiteLLM 网关监控仪表盘已预配置

## 健康检查

```bash
curl http://localhost:8000/health
# {"status":"ok","version":"1.0.0"}
```

## 参考项目

- [LiteLLM](https://github.com/BerriAI/litellm) - AI 网关核心
- [One API](https://github.com/songquanpeng/one-api) - API 管理参考
- [New API](https://github.com/QuantumNous/new-api) - One API 改进版
- [Bifrost](https://github.com/maximhq/bifrost) - 高性能 AI 网关参考
