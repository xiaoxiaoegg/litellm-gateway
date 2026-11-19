# 部署清单：把 Token 中转站部署到服务器给别人用

> 目标：把本机 `localhost:8000` 的 token 中转站，变成内网/公网上**别人也能用**、且带鉴权+计费的多用户服务。
> 配套文档：`7.md`（功能/客户端对接/计费原理）、`gateway-go/ARCHITECTURE.md`（架构）。

---

## 一、本机已完成的修复（无需在服务器上重做，但迁移时一并带走）

- [x] nginx `location /v1/` 指向 `app_backend`（gateway-go:8000），不再直连 litellm
- [x] nginx **新增 `location /api/` → app_backend**（原先缺失，别人用域名时后台/建 Key 会 404）
- [x] nginx `location /health` → app_backend
- [x] 多用户计费隔离（按 `user_id` 乐观锁扣费）
- [x] Codex `/v1/responses` 已接入计费（之前白嫖，已修）
- [x] 创建用户缺主键 ID 导致误报「已存在」（已修）

> ⚠️ 待办（不影响给别人用，但影响模型丰富度）：kimi Key 过期需更新；百度千帆 OAuth 未接入；通义等可按 `litellm-config.yaml` 自行加。

---

## 二、服务器准备

- [ ] 一台 Linux 服务器（内网或公网均可），装好 **Docker + docker compose**
- [ ] 把整个 `litellm-gateway/` 目录拷过去（含 `.env`、`nginx/`、`litellm-config.yaml`、`gateway-go/`、各 compose 服务）
- [ ] **只对外暴露 443**；`8000`(gateway)、`4000`(litellm)、`5432`(postgres)、`3000/9090`(Grafana/Prometheus) 只在 docker 网络内，防火墙**不要**对公网开放
- [ ] 网关改 Go 代码后必须重新交叉编译：`cd gateway-go && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o gateway . && cd .. && docker compose up -d --build gateway-go`（否则容器内还是旧二进制）

---

## 三、改密钥（上线前必做，安全红线）

- [ ] `.env` 里 `LITELLM_MASTER_KEY` 换成强随机串（网关内部转发用，别泄露）
- [ ] `docker-compose.yaml` 里 postgres 密码 `dbpassword9090` → 强密码（同步改 `.env` 的 `DATABASE_URL`）
- [ ] 后台管理员 `admin/admin123` → 改成强密码（登录后在「用户管理」改，或直连 DB 改 `users` 表）
- [ ] `DEEPSEEK_API_KEY` 等填**真实有效**的供应商 Key（当前填的是示例/真实值，上线前确认额度充足）
- [ ] **master key（`sk-cd8...`）绝不发给别人**——它绕过计费。别人只发他们自己的 `api_key`

---

## 四、域名 + HTTPS 证书

- [ ] 在 `nginx/conf.d/default.conf` 把两处 `server_name localhost` 改成你的域名（如 `relay.yourcorp.com`）
- [ ] 替换 `nginx/ssl/cert.pem` + `key.pem` 为真实证书（Let's Encrypt 或自建 CA）
  - Let's Encrypt：`certbot certonly` 后把 fullchain.pem / privkey.pem 复制覆盖即可
- [ ] 防火墙放行 `443`（以及 `80` 用于 ACME 验证 / 80→443 跳转，nginx 已配 80 跳转）
- [ ] 重启 nginx：`docker compose up -d nginx` 或 `docker compose exec nginx nginx -s reload`

---

## 五、启动 & 冒烟验证

```bash
cd litellm-gateway
docker compose up -d
# 等所有容器 healthy（gateway-go / litellm / db）
docker compose ps

# 1) 模型发现（应过网关返回模型列表）
curl -k https://你的域名/v1/models | head -c 200
# 2) 用 master key 打一次 deepseek（应返回 DeepSeek 回复）
curl -k -X POST https://你的域名/v1/responses \
  -H "Authorization: Bearer <master-key>" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-chat","input":"hi","max_output_tokens":20}'
```

---

## 六、给别人开账号（代建流程，推荐可控）

1. 浏览器开 `https://你的域名/login.html`，用 admin 登录
2. 调后台：`POST /api/admin/users` `{ "username":"user-a", "password":"...", "role":"user", "initial_balance":50 }`
3. 让对方登录 `https://你的域名/login.html` → `POST /api/keys` `{ "key_name":"my-key" }` → 拿到 `api_key`（**明文只返回一次，务必保存**）
   - 或你代建 Key 后把 `api_key` 发给他
4. 把「网关地址 + 他的 `api_key`」发给他，他按第七节配客户端即可
5. 余额用完前你 `POST /api/admin/users/:id/recharge` 充值；或在后台看「数据看板」对账

> 开放注册（方式 B）：`POST /api/auth/register` 自助注册，但余额=0，需你充值后才能用（否则 402）。

---

## 七、客户端配置

**Claude Code**
```bash
export ANTHROPIC_BASE_URL=https://你的域名/v1     # 不带 /messages，SDK 自动补
export ANTHROPIC_API_KEY=<他的api_key>
claude -p "ping" --model claude-opus-4-8
```

**Codex**（`~/.codex/config.toml`）
```toml
model_provider = "custom"
[model_providers.custom]
name = "relay"
base_url = "https://你的域名/v1"
wire_api = "responses"
requires_openai_auth = true
experimental_bearer_token = "<他的api_key>"
```
> ⚠️ Codex 用 `model_provider="custom"` 时只认 config.toml 里的 `base_url`，命令行 `OPENAI_*` 前缀被忽略；且**别开 CC Switch.app**，它会把配置改回直连真 deepseek。

---

## 八、端到端验证记录（2026-07-20 本机实测，可作为上线后对照）

| 步骤 | 操作 | 结果 |
|---|---|---|
| 1 | admin 登录 `POST /api/auth/login` | 拿到 `access_token` |
| 2 | 建用户 `POST /api/admin/users` initial_balance=100 | `balance=100` |
| 3 | 用户登录 → `POST /api/keys {key_name:"testkey"}` | 拿到 `api_key`（64 位） |
| 4 | 调用前 `GET /api/balance` | `100` |
| 5 | 用该 key 调 `POST /v1/responses` model=gpt-5.6-sol | 返回 DeepSeek 回复 |
| 6 | 调用后 `GET /api/balance` | `99.9939`（已扣费） |
| 7 | 错误 key 调 `POST /v1/responses` | `{"detail":"API Key 无效"}`（证明过网关鉴权） |

---

## 九、安全红线（务必遵守）

- [ ] **只暴露 443**，8000/4000/5432/3000/9090 不对公网开放
- [ ] **master key 不外流**（绕过计费）
- [ ] 上线前改掉所有默认口令（admin、DB 密码、litellm master key）
- [ ] 供应商真实 Key 在 `.env`，勿提交到 git
- [ ] 定期看后台「数据看板」做用量/费用对账
- [ ] 网关当前无 CORS 中间件：CLI 客户端（Claude Code/Codex）不受影响；若以后做 web 跨域前端需再加
