#!/bin/bash

echo "============================================"
echo "   模型测试 - 逐个验证 LiteLLM 模型"
echo "============================================"

LITELLM_URL="${LITELLM_URL:-http://localhost:4000}"
API_KEY="${API_KEY:-sk-1qaz@WSX@123456}"

MODELS=(
  "glm-4-flash"
  "glm-4-flash-fast"
  "deepseek-chat"
  "kimi"
  "ernie-bot"
)

PASS=0
FAIL=0

for MODEL in "${MODELS[@]}"; do
  echo ""
  echo "--- 测试: $MODEL ---"
  OUTPUT=$(curl -s -w "\n%{http_code}" -X POST "$LITELLM_URL/chat/completions" \
    -H "Authorization: Bearer $API_KEY" \
    -H "Content-Type: application/json" \
    -d "{
      \"model\": \"$MODEL\",
      \"messages\": [{\"role\": \"user\", \"content\": \"你好，请简单回复'hello'\"}],
      \"max_tokens\": 50
    }" 2>&1)
  
  HTTP_CODE=$(echo "$OUTPUT" | tail -1)
  BODY=$(echo "$OUTPUT" | sed '$d')
  
  if [ "$HTTP_CODE" = "200" ]; then
    REPLY=$(echo "$BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['choices'][0]['message']['content'][:80])" 2>/dev/null)
    echo "  [OK] HTTP $HTTP_CODE -> $REPLY"
    ((PASS++))
  else
    ERROR=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('error',{}).get('message','unknown error')[:100])" 2>/dev/null)
    echo "  [FAIL] HTTP $HTTP_CODE -> $ERROR"
    ((FAIL++))
  fi
done

echo ""
echo "============================================"
echo "  结果: $PASS 通过, $FAIL 失败"
echo "============================================"
