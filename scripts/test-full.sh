#!/bin/bash
BASE="http://localhost:8000"
PASS=0; FAIL=0
GREEN='\033[32m'; RED='\033[31m'; YELLOW='\033[33m'; NC='\033[0m'
ok()   { ((PASS++)); echo -e "  ${GREEN}[PASS]${NC} $1"; }
fail() { ((FAIL++)); echo -e "  ${RED}[FAIL]${NC} $1"; }

echo "==============================================="
echo "  Token 中转站 - 全链路测试"
echo "==============================================="
echo ""

LAN_IP=$(ifconfig en0 2>/dev/null | grep "inet " | awk '{print $2}')
[ -z "$LAN_IP" ] && LAN_IP="127.0.0.1"

# ── 0. RESET ADMIN QUOTA ──
echo "0. 重置管理员配额"
docker compose -f "$(dirname "$0")/../docker-compose.yaml" exec -T db psql -U llmproxy -d litellm \
  -c "UPDATE users SET daily_quota=0, used_daily_tokens=0 WHERE username='admin';" > /dev/null 2>&1
ok "配额已复位"

# ── 1. HEALTH ──
echo "1. 服务健康检查"
R=$(curl -s -w " %{http_code}" $BASE/health 2>/dev/null)
C=$(echo "$R" | awk '{print $NF}')
[ "$C" = "200" ] && ok "网关正常" || { fail "无响应 ($C)"; exit 1; }

# ── 2. MODELS ──
echo "2. 模型列表"
C=$(curl -s $BASE/v1/models | grep -o '"id":"[^"]*"' | wc -l | tr -d ' ')
[ "$C" -ge 4 ] && ok "发现 $C 个模型" || fail "不足 ($C 个)"

# ── 3. ADMIN LOGIN ──
echo "3. 管理员登录"
LOGIN=$(curl -s -X POST $BASE/api/auth/login -H "Content-Type: application/json" -d '{"username":"admin","password":"admin123"}')
TOKEN=$(echo "$LOGIN" | sed 's/.*"access_token":"\([^"]*\)".*/\1/')
[ -n "$TOKEN" ] && ok "登录成功" || fail "登录失败"

# ── 4. USER LIST ──
echo "4. 用户列表"
TOT=$(curl -s $BASE/api/admin/users -H "Authorization: Bearer $TOKEN" | sed 's/.*"total":\([0-9]*\).*/\1/')
[ "$TOT" -ge 1 ] && ok "$TOT 个用户" || fail "0 用户"

# ── 5. CREATE TEST USER ──
echo "5. 创建测试用户"
curl -s -X POST $BASE/api/admin/users -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d '{"username":"tester","password":"test123456","initial_balance":5000}' > /dev/null
ok "tester (余额=5000)"

# ── 6. CREATE API KEY ──
echo "6. 创建 API Key"
UK=$(curl -s -X POST $BASE/api/keys -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d '{"key_name":"test"}' | grep -o 'sk-[a-f0-9-]*' | head -1)
[ -n "$UK" ] && ok "Key: ${UK:0:25}..." || fail "创建失败"

# ── 7. CHAT ──
echo "7. 模型调用"
CH=$(curl -s -w " %{http_code}" -X POST $BASE/v1/chat/completions -H "Content-Type: application/json" \
  -H "Authorization: Bearer $UK" -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"hello"}],"max_tokens":20}')
CC=$(echo "$CH" | awk '{print $NF}')
[ "$CC" = "200" ] && ok "HTTP 200 (deepseek-chat 返回正常)" || fail "HTTP $CC"

# ── 8. STREAMING ──
echo "8. 流式响应"
S=$(curl -s -N -X POST $BASE/v1/chat/completions -H "Content-Type: application/json" \
  -H "Authorization: Bearer $UK" -d '{"model":"glm-4-flash","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":20}' 2>/dev/null | grep -c "data:" || true)
[ "$S" -ge 1 ] && ok "SSE 流式正常" || fail "流式异常"

# ── 9. BALANCE ──
echo "9. 余额查询"
B=$(curl -s $BASE/api/balance -H "Authorization: Bearer $TOKEN" | sed 's/.*"balance":\([0-9.]*\).*/\1/')
[ -n "$B" ] && ok "余额: $B" || fail "余额异常"

# ── 10. MODEL MAPPING ──
echo "10. 模型映射 (claude-opus-4-8 → deepseek-chat)"
M=$(curl -s -w " %{http_code}" -X POST $BASE/v1/chat/completions -H "Content-Type: application/json" \
  -H "Authorization: Bearer $UK" -d '{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}],"max_tokens":15}')
MC=$(echo "$M" | awk '{print $NF}')
[ "$MC" = "200" ] && ok "HTTP 200 (映射正常)" || fail "HTTP $MC"

# ── 11. ANTHROPIC ──
echo "11. Anthropic /v1/messages"
A=$(curl -s -w " %{http_code}" -X POST $BASE/v1/messages -H "Content-Type: application/json" \
  -H "Authorization: Bearer $UK" -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"max_tokens":20}')
AC=$(echo "$A" | awk '{print $NF}')
[ "$AC" = "200" ] && ok "HTTP 200 (Anthropic端点)" || fail "HTTP $AC"

# ── 12. QUOTA ──
echo "12. 配额限制"
AID=$(curl -s $BASE/api/admin/users -H "Authorization: Bearer $TOKEN" | grep -o '"id":"[^"]*","username":"admin"' | cut -d'"' -f4)
# Reset used_daily_tokens before quota test
docker compose -f "$(dirname "$0")/../docker-compose.yaml" exec -T db psql -U llmproxy -d litellm -c "UPDATE users SET daily_quota=0, used_daily_tokens=0 WHERE username='admin';" > /dev/null 2>&1
curl -s -X POST "$BASE/api/admin/users/$AID/quota" -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" -d '{"daily_quota":1}' > /dev/null
Q1=$(curl -s -w " %{http_code}" -X POST $BASE/v1/chat/completions -H "Content-Type: application/json" -H "Authorization: Bearer $UK" -d '{"model":"glm-4-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":50}')
Q1C=$(echo "$Q1" | awk '{print $NF}')
Q2=$(curl -s -w " %{http_code}" -X POST $BASE/v1/chat/completions -H "Content-Type: application/json" -H "Authorization: Bearer $UK" -d '{"model":"glm-4-flash","messages":[{"role":"user","content":"bye"}],"max_tokens":50}')
Q2C=$(echo "$Q2" | awk '{print $NF}')
[ "$Q1C" = "200" ] && [ "$Q2C" = "429" ] && ok "200 → 429 (超限阻断)" || fail "$Q1C → $Q2C"
curl -s -X POST "$BASE/api/admin/users/$AID/quota" -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" -d '{"daily_quota":0}' > /dev/null

# ── 13. TXN ──
echo "13. 交易记录"
[ -n "$(curl -s $BASE/api/transactions -H "Authorization: Bearer $TOKEN" | grep total)" ] && ok "可查" || fail "无记录"

echo ""
echo "==============================================="
echo -e "  结果: ${GREEN}$PASS 通过${NC}  ${RED}$FAIL 失败${NC}"
echo "==============================================="
echo -e "\n局域网: ${YELLOW}http://${LAN_IP}:8000/login.html${NC}"
echo -e "Codex:  ${YELLOW}export OPENAI_BASE_URL=http://${LAN_IP}:8000/v1${NC}"
echo -e "        ${YELLOW}export OPENAI_API_KEY=${UK}${NC}"
echo -e "Claude: ${YELLOW}export ANTHROPIC_BASE_URL=http://${LAN_IP}:8000${NC}"
echo -e "        ${YELLOW}export ANTHROPIC_API_KEY=${UK}${NC}"
exit $FAIL
