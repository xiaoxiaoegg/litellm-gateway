#!/bin/bash
set -e

echo "============================================"
echo "  Token 中转站 - API 测试脚本"
echo "============================================"

BASE="http://localhost:8000"
TOKEN=""
API_KEY=""

echo ""
echo "1. 健康检查"
echo "----------------------------------------"
curl -s "$BASE/health" | python3 -m json.tool

echo ""
echo "2. 注册新用户"
echo "----------------------------------------"
REG=$(curl -s -X POST "$BASE/api/auth/register" \
  -H "Content-Type: application/json" \
  -d '{"username":"testuser","password":"test123456"}')
echo "$REG" | python3 -m json.tool
TOKEN=$(echo "$REG" | python3 -c "import sys,json; print(json.load(sys.stdin).get('access_token',''))")

echo ""
echo "3. 登录"
echo "----------------------------------------"
LOGIN=$(curl -s -X POST "$BASE/api/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"username":"testuser","password":"test123456"}')
echo "$LOGIN" | python3 -m json.tool

echo ""
echo "4. 查看个人信息"
echo "----------------------------------------"
curl -s "$BASE/api/auth/me" -H "Authorization: Bearer $TOKEN" | python3 -m json.tool

echo ""
echo "5. 创建 API Key"
echo "----------------------------------------"
KEY_RESULT=$(curl -s -X POST "$BASE/api/keys" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"key_name":"测试 Key"}')
echo "$KEY_RESULT" | python3 -m json.tool
API_KEY=$(echo "$KEY_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('api_key',''))")

echo ""
echo "6. 用 API Key 调用聊天（余额不足应失败）"
echo "----------------------------------------"
curl -s -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-4-flash","messages":[{"role":"user","content":"你好"}]}' | python3 -m json.tool

echo ""
echo "7. 管理员登录并给用户充值"
echo "----------------------------------------"
ADMIN_LOGIN=$(curl -s -X POST "$BASE/api/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"admin123"}')
ADMIN_TOKEN=$(echo "$ADMIN_LOGIN" | python3 -c "import sys,json; print(json.load(sys.stdin).get('access_token',''))")

# Get user ID
USER_ID=$(curl -s "$BASE/api/auth/me" -H "Authorization: Bearer $TOKEN" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))")
curl -s -X POST "$BASE/api/admin/users/$USER_ID/recharge" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"amount":10000,"description":"新手充值"}' | python3 -m json.tool

echo ""
echo "8. 再次用 API Key 调用聊天"
echo "----------------------------------------"
curl -s -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-4-flash","messages":[{"role":"user","content":"你好，介绍一下你自己"}]}' | python3 -m json.tool

echo ""
echo "9. 查看交易记录"
echo "----------------------------------------"
curl -s "$BASE/api/transactions" -H "Authorization: Bearer $TOKEN" | python3 -m json.tool

echo ""
echo "10. 查看使用记录"
echo "----------------------------------------"
curl -s "$BASE/api/usage" -H "Authorization: Bearer $TOKEN" | python3 -m json.tool

echo ""
echo "============================================"
echo "  测试完成"
echo "============================================"
