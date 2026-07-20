package handlers

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"token-gateway/database"
	"token-gateway/models"
)

// getEnvURL 返回上游 LiteLLM 代理地址。默认 docker 网络里的 http://litellm:4000，
// 可用环境变量 LITELLM_BASE_URL 覆盖（本地调试时指向本机端口）。
func getEnvURL() string {
	u := os.Getenv("LITELLM_BASE_URL")
	if u == "" {
		u = "http://litellm:4000"
	}
	return u
}

// completionReq 是 OpenAI chat/completions 请求结构体（只取本网关关心的字段）。
// 用 any 承载 Content / Tools，因为客户端可能发字符串也可能发数组（工具调用场景）。
type completionReq struct {
	Model               string    `json:"model"`
	Messages            []message `json:"messages"`
	Stream              bool      `json:"stream"`
	MaxTokens           int       `json:"max_tokens"`
	MaxCompletionTokens int       `json:"max_completion_tokens,omitempty"` // 推理模型（o1/o3/o4）用这个字段
	Tools               []any     `json:"tools,omitempty"`
	ToolChoice          any       `json:"tool_choice,omitempty"`
}

// effectiveMaxTokens 统一取 max_tokens。
// 普通模型用 max_tokens；推理模型（o1/o3/o4）用 max_completion_tokens。
// 两者取其一（优先 max_tokens），返回 0 表示上游用默认值。
func effectiveMaxTokens(req completionReq) int {
	if req.MaxTokens > 0 {
		return req.MaxTokens
	}
	if req.MaxCompletionTokens > 0 {
		return req.MaxCompletionTokens
	}
	return 0
}

// toolCall 描述一个工具调用（assistant 返回的 function call）。
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// message 是 OpenAI 单条对话消息，支持文本、工具调用、工具结果三种形态。
type message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// completionResp 是上游 OpenAI 响应（只取需要的字段：choices[].message 与 usage）。
type completionResp struct {
	ID      string `json:"id"`
	Choices []struct {
		Message struct {
			Role      string     `json:"role"`
			Content   any        `json:"content"`
			ToolCalls []toolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// ChatCompletion 是 POST /v1/chat/completions 主入口（OpenAI 协议，Codex 传统路径）。
// 流程：解析请求 → 模型映射 → 流式走 handleStream / 非流式走 tryUpstreamKeys 或 callLiteLLM → 计费返回。
func ChatCompletion(c *gin.Context) {
	var req completionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"detail": "请求格式错误"})
		return
	}

	// 把客户端模型名翻译成实际上游模型名，并记录日志方便排查映射问题。
	originalModel := req.Model
	req.Model = ResolveModel(req.Model)
	if req.Model != originalModel {
		log.Printf("[MODEL_MAP] %s -> %s", originalModel, req.Model)
	}
	maxTokens := effectiveMaxTokens(req)

	if req.Stream {
		handleStream(c, req, maxTokens)
		return
	}

	// 优先尝试"用户配置的上游 Key 直连"（千帆或管理员配置的上游），命中就直接返回。
	body, usedUpstream := tryUpstreamKeys(req)
	if usedUpstream {
		finishChat(c, req, body)
		return
	}

	// 否则走默认 LiteLLM 上游（用 master key 内部转发）。
	body, llmErr := callLiteLLM(req)
	if llmErr != nil {
		c.JSON(llmErr.Code, llmErr.Msg)
		return
	}
	finishChat(c, req, body)
}

// handleStream 处理流式请求：把上游 SSE 流透传给客户端。
// 这里只是字节级转发（不解析内容），所以不区分模型协议细节，统一把 data: 行原样写给客户端。
func handleStream(c *gin.Context, req completionReq, maxTokens int) {
	payload, _ := json.Marshal(map[string]interface{}{
		"model": req.Model, "messages": req.Messages, "stream": true,
		"max_tokens": maxTokens, "tools": req.Tools, "tool_choice": req.ToolChoice,
	})

	httpReq, _ := http.NewRequest("POST", fmt.Sprintf("%s/chat/completions", getEnvURL()), bytes.NewReader(payload))
	// 注意：打上游用的是"内部 master key"，不是客户端带来的 Key。
	httpReq.Header.Set("Authorization", "Bearer sk-1qaz@WSX@123456")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		c.JSON(502, gin.H{"detail": fmt.Sprintf("LiteLLM: %v", err)})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		c.Data(resp.StatusCode, "application/json", body)
		return
	}

	// 设置 SSE 响应头并把上游流逐行转发。流式响应不在此计费/写用量（简单起见，流式不计费）。
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.WriteHeader(http.StatusOK)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	flusher, _ := c.Writer.(http.Flusher)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			c.Writer.Write([]byte("\n"))
			if flusher != nil {
				flusher.Flush()
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			c.Writer.Write([]byte(line + "\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// httpErr 是统一的错误结构（HTTP 码 + JSON 错误体）。
type httpErr struct {
	Code int
	Msg  gin.H
}

// callLiteLLM 用内部 master key 调上游 LiteLLM 的 chat/completions，返回原始响应体或错误。
func callLiteLLM(req completionReq) ([]byte, *httpErr) {
	payload, _ := json.Marshal(map[string]interface{}{
		"model": req.Model, "messages": req.Messages,
		"max_tokens": effectiveMaxTokens(req), "tools": req.Tools, "tool_choice": req.ToolChoice,
	})
	httpReq, _ := http.NewRequest("POST", fmt.Sprintf("%s/chat/completions", getEnvURL()), bytes.NewReader(payload))
	httpReq.Header.Set("Authorization", "Bearer sk-1qaz@WSX@123456")
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, &httpErr{502, gin.H{"detail": "LiteLLM: " + err.Error()}}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, &httpErr{resp.StatusCode, gin.H{"detail": string(body)}}
	}
	return body, nil
}

// tryUpstreamKeys 尝试用"用户配置的上游 Key"直连供应商，绕过 LiteLLM。
//  1. 千帆模型走专门的 OAuth + 直连（见 qianfan.go）。
//  2. 否则按 model_name 查 UpstreamKey 表，按 priority 升序逐个尝试，命中 200 即返回。
//     遇到 401/403/429 自动禁用该 Key，避免反复重试失效 Key。
//
// 返回 (body, true) 表示命中上游直连；(nil, false) 表示没有可用上游，交由 callLiteLLM 兜底。
func tryUpstreamKeys(req completionReq) ([]byte, bool) {
	// 千帆模型优先走专门直连逻辑。
	if isQianfanModel(req.Model) {
		if body, ok := tryQianfan(req.Model, req.Messages); ok {
			return body, true
		}
	}
	var keys []models.UpstreamKey
	database.DB.Where("model_name = ? AND is_active = true", req.Model).Order("priority asc").Find(&keys)
	if len(keys) == 0 {
		return nil, false
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"model": req.Model, "messages": req.Messages, "max_tokens": effectiveMaxTokens(req),
	})

	for _, key := range keys {
		baseURL := key.BaseURL
		if baseURL == "" {
			baseURL = defaultBaseURL(req.Model) // 没配 base url 时用内置供应商默认地址
		}
		r, _ := http.NewRequest("POST", fmt.Sprintf("%s/chat/completions", baseURL), bytes.NewReader(payload))
		r.Header.Set("Authorization", "Bearer "+key.ApiKey)
		r.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			continue // 网络错误尝试下一个 Key
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			database.DB.Model(&key).Update("used_quota", key.UsedQuota+1)
			return body, true
		}
		// 鉴权/限流类错误：禁用该 Key，避免后面请求继续踩坑。
		if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 429 {
			database.DB.Model(&key).Update("is_active", false)
		}
	}
	return nil, false
}

// defaultBaseURL 返回各模型对应的供应商默认 base url（当 UpstreamKey.BaseURL 为空时使用）。
func defaultBaseURL(model string) string {
	switch model {
	case "ernie-bot", "ernie-bot-turbo", "ernie-4.0-8k", "ernie-3.5-8k", "ernie-speed", "ernie-lite":
		return "https://aip.baidubce.com/rpc/2.0/ai_custom/v1/wenxinworkshop/chat"
	case "qwen-max", "qwen-plus", "qwen-turbo":
		return "https://dashscope.aliyuncs.com/compatible-mode/v1"
	case "glm-4-flash", "glm-4-flash-fast":
		return "https://open.bigmodel.cn/api/paas/v4"
	case "deepseek-chat":
		return "https://api.deepseek.com/v1"
	case "kimi":
		return "https://api.moonshot.cn/v1"
	default:
		return "https://api.openai.com/v1"
	}
}

// finishChat 是"非流式"响应的统一收口：做每日配额校验、计费扣款、用量/交易记录，最后把响应返回客户端。
//
// 计费只在非 master 用户上生效（master key 免费用）。计费用乐观锁 SQL
// （balance - cost WHERE balance >= cost），若余额不足则 RowsAffected==0，返回 402。
// chargeForUsage 是非 master 用户的计费核心（被 /v1/chat/completions、/v1/messages、
// /v1/responses 三条代理路径共用，确保计费逻辑一致）。
//
// 流程：
//  1. master key 直通（is_master==true）直接放行、不计费、不写记录。
//  2. 取上下文里的 user_id / api_key_id（由 APIKeyAuth 注入）。
//  3. 每日配额滚动重置 + 上限校验（超限返回 429）。
//  4. 按模型定价计算 cost；cost<=0（无定价）放行不计费。
//  5. 乐观锁扣费（balance - cost WHERE balance >= cost），扣失败（余额不足）返回 402。
//  6. 写 UsageRecord（用量明细）+ Transaction（扣费流水），余额 <100 时打低余额警告头。
//
// 返回 (status, detail)：status==200 表示放行（含计费成功），否则调用方应以其中止响应。
func chargeForUsage(c *gin.Context, model string, promptTokens, completionTokens int) (int, string) {
	// 设计要点：master key 直通，平台超级 Key 不扣费。
	isMaster, _ := c.Get("is_master")
	if isMaster != nil && isMaster.(bool) {
		return 200, ""
	}
	// 普通用户必须从上下文拿到合法 user_id，否则无法计费。
	uid, _ := c.Get("user_id")
	uidStr, ok := uid.(string)
	if !ok || uidStr == "" {
		return 403, "缺少用户上下文"
	}
	userID, err := uuid.Parse(uidStr)
	if err != nil {
		return 403, "用户标识无效"
	}
	aid, _ := c.Get("api_key_id")
	var apiKeyID uuid.UUID
	if aidStr, ok := aid.(string); ok && aidStr != "" {
		apiKeyID, _ = uuid.Parse(aidStr)
	}

	var user models.User
	if database.DB.First(&user, "id = ?", userID).Error != nil {
		return 403, "用户不存在"
	}
	// 每日配额滚动重置：越过 QuotaResetAt 则清零已用量并推后一天。
	today := time.Now().Truncate(24 * time.Hour)
	if user.QuotaResetAt.Before(today) {
		database.DB.Model(&user).Updates(map[string]interface{}{
			"used_daily_tokens": 0, "quota_reset_at": today.Add(24 * time.Hour),
		})
		user.UsedDailyTokens = 0
	}
	if user.DailyQuota > 0 && user.UsedDailyTokens >= user.DailyQuota {
		return 429, "每日用量已达上限"
	}

	cost := calculateCost(model, promptTokens, completionTokens)
	if cost <= 0 {
		return 200, "" // 查不到定价（新模型未配置价格）放行不计费
	}
	totalTokens := promptTokens + completionTokens
	// 乐观锁扣费：只有余额足够时才扣，否则不扣（避免并发超卖）。
	r := database.DB.Exec(
		"UPDATE users SET balance = balance - ?, used_daily_tokens = used_daily_tokens + ? WHERE id = ? AND balance >= ?",
		cost, totalTokens, userID, cost,
	)
	if r.RowsAffected == 0 {
		return 402, "余额不足，请充值"
	}
	// 写用量明细（统计看板/对账用）。
	database.DB.Create(&models.UsageRecord{
		ID: uuid.New(), UserID: userID, ApiKeyID: &apiKeyID, ModelName: model,
		PromptTokens: promptTokens, CompletionTokens: completionTokens,
		TotalTokens: totalTokens, Cost: cost, RequestID: fmt.Sprintf("resp_%s", uuid.New().String()[:12]),
	})
	// 写交易流水（负金额表示扣费）。
	database.DB.Create(&models.Transaction{
		ID: uuid.New(), UserID: userID, Amount: -cost, Type: "deduct",
		Description: fmt.Sprintf("%s: %d tokens", model, totalTokens),
	})
	// 余额低于 100 给客户端一个低余额警告头，前端可据此提示充值。
	if database.DB.First(&user, "id = ?", userID).Error == nil && user.Balance < 100 {
		c.Header("X-Balance-Warning", "low")
	}
	return 200, ""
}

func finishChat(c *gin.Context, req completionReq, body []byte) {
	isMaster, _ := c.Get("is_master")
	// 仅对非 master 用户做配额/计费；master 直通不扣费。
	if isMaster != nil && isMaster.(bool) == false {
		uid, _ := c.Get("user_id")
		userID, _ := uuid.Parse(uid.(string))
		var user models.User
		if database.DB.First(&user, "id = ?", userID).Error == nil {
			// 每日配额滚动重置：若已越过 QuotaResetAt，则清零已用量并把重置时间推后一天。
			today := time.Now().Truncate(24 * time.Hour)
			if user.QuotaResetAt.Before(today) {
				database.DB.Model(&user).Updates(map[string]interface{}{
					"used_daily_tokens": 0, "quota_reset_at": today.Add(24 * time.Hour),
				})
				user.UsedDailyTokens = 0
			}
			// 配置了每日上限且已达上限：拒绝（429）。
			if user.DailyQuota > 0 && user.UsedDailyTokens >= user.DailyQuota {
				c.JSON(429, gin.H{"detail": "每日用量已达上限", "quota": user.DailyQuota, "used": user.UsedDailyTokens})
				return
			}
		}
	}

	// 解析上游响应，重写一个平台自己的响应 ID（方便对账/日志）。
	var result completionResp
	json.Unmarshal(body, &result)
	result.ID = fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:12])

	// 非 master 用户才计费。
	if isMaster != nil && isMaster.(bool) == false {
		uid, _ := c.Get("user_id")
		aid, _ := c.Get("api_key_id")
		userID, _ := uuid.Parse(uid.(string))
		apiKeyID, _ := uuid.Parse(aid.(string))
		cost := calculateCost(req.Model, result.Usage.PromptTokens, result.Usage.CompletionTokens)
		if cost > 0 {
			totalTokens := int64(result.Usage.TotalTokens)
			// 乐观锁扣费：只有在余额足够时才扣，否则不扣（避免并发超卖）。
			r := database.DB.Exec(
				"UPDATE users SET balance = balance - ?, used_daily_tokens = used_daily_tokens + ? WHERE id = ? AND balance >= ?",
				cost, totalTokens, userID, cost,
			)
			if r.RowsAffected == 0 {
				c.JSON(402, gin.H{"detail": "余额不足"})
				return
			}
			// 写用量明细。
			database.DB.Create(&models.UsageRecord{
				ID: uuid.New(), UserID: userID, ApiKeyID: &apiKeyID, ModelName: req.Model,
				PromptTokens: result.Usage.PromptTokens, CompletionTokens: result.Usage.CompletionTokens,
				TotalTokens: result.Usage.TotalTokens, Cost: cost, RequestID: result.ID,
			})
			// 写交易流水（负金额表示扣费）。
			database.DB.Create(&models.Transaction{
				ID: uuid.New(), UserID: userID, Amount: -cost, Type: "deduct",
				Description: fmt.Sprintf("%s: %d tokens", req.Model, result.Usage.TotalTokens),
			})
			// 余额低于 100 时给客户端一个低余额警告头，前端可据此提示充值。
			var user models.User
			if database.DB.First(&user, "id = ?", userID).Error == nil && user.Balance < 100 {
				c.Header("X-Balance-Warning", "low")
			}
		}
	}
	respBytes, _ := json.Marshal(result)
	c.Data(200, "application/json", respBytes)
}

// calculateCost 按模型定价计算一次请求花费：单价(每千token) × token数 / 1000。
// 查不到定价（例如新模型没配置价格）返回 0，即不计费。
func calculateCost(modelName string, prompt, completion int) float64 {
	var p models.ModelPricing
	if database.DB.Where("model_name = ?", modelName).First(&p).Error != nil {
		return 0
	}
	return float64(prompt)/1000.0*p.PromptPricePer1k + float64(completion)/1000.0*p.CompletionPricePer1k
}
