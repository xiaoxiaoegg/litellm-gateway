package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// 千帆（百度文心）的 access_token 需要单独用 OAuth 申请，且有有效期。
// 这里用包级变量缓存 token，并加互斥锁保证并发安全；过期前复用，避免每次请求都申请。
var (
	qianfanToken    string
	qianfanTokenExp time.Time
	qianfanTokenMu  sync.Mutex
)

// getQianfanAccessToken 获取（或复用缓存的）千帆 access_token。
// 优先用环境变量 QIANFAN_ACCESS_KEY / QIANFAN_SECRET_KEY 做 client_credentials 授权。
// token 提前 5 分钟刷新（ExpiresIn-300），防止临界过期。
func getQianfanAccessToken() (string, error) {
	qianfanTokenMu.Lock()
	defer qianfanTokenMu.Unlock()

	// 缓存命中且未过期，直接返回。
	if qianfanToken != "" && time.Now().Before(qianfanTokenExp) {
		return qianfanToken, nil
	}

	ak := os.Getenv("QIANFAN_ACCESS_KEY")
	sk := os.Getenv("QIANFAN_SECRET_KEY")
	if ak == "" || sk == "" {
		return "", fmt.Errorf("QIANFAN_ACCESS_KEY or QIANFAN_SECRET_KEY not set")
	}

	url := fmt.Sprintf("https://aip.baidubce.com/oauth/2.0/token?grant_type=client_credentials&client_id=%s&client_secret=%s", ak, sk)
	resp, err := http.DefaultClient.Get(url)
	if err != nil {
		return "", fmt.Errorf("qianfan oauth request failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("qianfan oauth parse failed: %v", err)
	}
	if result.Error != "" {
		return "", fmt.Errorf("qianfan oauth error: %s - %s", result.Error, result.ErrorDesc)
	}

	qianfanToken = result.AccessToken
	qianfanTokenExp = time.Now().Add(time.Duration(result.ExpiresIn-300) * time.Second) // 提前 5 分钟刷新
	log.Printf("[QIANFAN] OAuth token acquired, expires in %ds", result.ExpiresIn)
	return qianfanToken, nil
}

// qianfanModelMap：平台内部模型名 → 百度文心 API 的端点路径（URL 里的服务名）。
// 例如 ernie-bot 对应 completions_pro。未知模型回退到 "completions"。
var qianfanModelMap = map[string]string{
	"ernie-bot":       "completions_pro",
	"ernie-bot-turbo": "eb-instant",
	"ernie-4.0-8k":    "completions_pro",
	"ernie-3.5-8k":    "completions",
	"ernie-speed":     "ernie_speed",
	"ernie-lite":      "ernie-lite",
}

// qianfanReq / qianfanMsg / qianfanResp 是千帆 API 的请求/响应结构（只取需要的字段）。
type qianfanReq struct {
	Messages []qianfanMsg `json:"messages"`
	Stream   bool         `json:"stream,omitempty"`
}

type qianfanMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type qianfanResp struct {
	ID     string `json:"id"`
	Result string `json:"result"`
	Usage  struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// tryQianfan 直接调用千帆（不走 LiteLLM）：申请 token → 组装请求 → 转发 → 把响应转成 OpenAI 兼容格式返回。
// 成功返回 (body, true)；任何环节失败（没配 key / 网络错 / 非 200 / 解析失败）返回 (nil, false)，
// 调用方会再 fallback 到 LiteLLM 默认上游。
func tryQianfan(model string, messages []message) ([]byte, bool) {
	token, err := getQianfanAccessToken()
	if err != nil {
		log.Printf("[QIANFAN] token error: %v", err)
		return nil, false
	}

	// 取模型对应的百度端点路径，未知则回退 completions。
	endpoint, ok := qianfanModelMap[model]
	if !ok {
		endpoint = "completions"
	}

	url := fmt.Sprintf("https://aip.baidubce.com/rpc/2.0/ai_custom/v1/wenxinworkshop/chat/%s?access_token=%s", endpoint, token)

	// 把平台统一的 message 结构转成千帆的 {role, content} 字符串形态。
	qmsgs := make([]qianfanMsg, len(messages))
	for i, m := range messages {
		content := ""
		switch v := m.Content.(type) {
		case string:
			content = v
		default:
			// 非字符串内容（如工具结果数组）直接 JSON 化塞进去。
			b, _ := json.Marshal(v)
			content = string(b)
		}
		qmsgs[i] = qianfanMsg{Role: m.Role, Content: content}
	}

	payload, _ := json.Marshal(qianfanReq{Messages: qmsgs})
	req, _ := http.NewRequest("POST", url, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[QIANFAN] request error: %v", err)
		return nil, false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		log.Printf("[QIANFAN] API error %d: %s", resp.StatusCode, truncate(string(body), 200))
		return nil, false
	}

	var qr qianfanResp
	if err := json.Unmarshal(body, &qr); err != nil {
		log.Printf("[QIANFAN] parse error: %v", err)
		return nil, false
	}

	// 把千帆响应转成 OpenAI 兼容格式，这样下游 finishChat/计费逻辑能统一处理。
	openaiResp := map[string]interface{}{
		"id":     qr.ID,
		"object": "chat.completion",
		"model":  model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": qr.Result,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     qr.Usage.PromptTokens,
			"completion_tokens": qr.Usage.CompletionTokens,
			"total_tokens":      qr.Usage.TotalTokens,
		},
	}
	converted, _ := json.Marshal(openaiResp)
	return converted, true
}

// truncate 把日志字符串截断到 maxLen，避免把大段错误体刷进日志。
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// isQianfanModel 判断某模型是否属于千帆系（用于 tryUpstreamKeys 优先走直连）。
func isQianfanModel(model string) bool {
	_, ok := qianfanModelMap[model]
	return ok
}
