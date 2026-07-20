package handlers

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// 本文件实现 Anthropic Messages API（/v1/messages）与 OpenAI chat/completions 的双向转换。
//
// 为什么需要它：Claude Code 原生走 Anthropic 协议（system 是顶层字段、消息 content 是 block 数组、
// 工具叫 tool_use/tool_result），而本平台上游是 OpenAI 兼容格式。网关在这里做"翻译"，
// 让 Claude Code 以为自己在跟真 Anthropic 对话，实际请求被转成 OpenAI 格式打到 LiteLLM。
//
// 两个方向：
//   - 入站：anthropicReq → openai messages（buildOpenAIMessages / convertTools / convertToolChoice）
//   - 出站：OpenAI 响应 → anthropic 响应（openAIToAnthropic / streamAnthropic）

// ── Anthropic 请求结构（兼容 content 数组 + 工具）──
// content 用 json.RawMessage 是因为它既可能是字符串也可能是 block 数组，延迟解析更灵活。

type anthropicBlock struct {
	Type      string          `json:"type"` // "text" / "tool_use" / "tool_result" / "thinking" ...
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`          // tool_use 的 id
	Name      string          `json:"name,omitempty"`        // tool_use 的函数名
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use 的参数（JSON 对象）
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result 对应的 tool_use id
	Content   json.RawMessage `json:"content,omitempty"`     // tool_result 的结果内容
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicReq struct {
	Model         string          `json:"model"`
	Messages      []anthropicMsg  `json:"messages"`
	System        json.RawMessage `json:"system,omitempty"` // Anthropic 把 system 放在顶层，OpenAI 放在 messages 里
	MaxTokens     int             `json:"max_tokens"`
	Stream        bool            `json:"stream"`
	Tools         []anthropicTool `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
}

// ── OpenAI 中间结构 ──

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
}

// ── 工具定义：Anthropic -> OpenAI ──
// Anthropic 工具是扁平的 {name, description, input_schema}，OpenAI 要包一层 {type:"function", function:{...}}。

func convertTools(tools []anthropicTool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		params := t.InputSchema
		if len(params) == 0 {
			params = json.RawMessage(`{}`) // 没有 schema 时给个空对象，避免上游报错
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  params,
			},
		})
	}
	return out
}

// ── tool_choice：Anthropic -> OpenAI ──
// Anthropic: "any" → OpenAI "required"；"tool" → 指定函数；"none" → 不带 tool_choice；其余透传。

func convertToolChoice(rc json.RawMessage) any {
	if len(rc) == 0 {
		return nil
	}
	var c struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rc, &c); err != nil {
		return nil
	}
	switch c.Type {
	case "any":
		return map[string]any{"type": "required"}
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": c.Name}}
	case "none":
		return nil
	default: // auto / 其他
		return map[string]any{"type": c.Type}
	}
}

// ── 把 Anthropic system 提取为文本 ──
// system 可能是字符串，也可能是 [{type:"text",text:...}] 数组，这里统一抽成纯文本。

func systemToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// ── 把单条 message 的 content 拆成 文本 / tool_use / tool_result ──

func parseContent(raw json.RawMessage) (text string, toolUses []anthropicBlock, toolResults []anthropicBlock) {
	if len(raw) == 0 {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		text = s
		return
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) == nil {
		for _, blk := range blocks {
			switch blk.Type {
			case "text":
				text += blk.Text
			case "tool_use":
				toolUses = append(toolUses, blk)
			case "tool_result":
				toolResults = append(toolResults, blk)
			case "thinking", "redacted_thinking":
				// 忽略推理块（OpenAI 侧不需要）
			}
		}
	}
	return
}

// ── 构造 OpenAI messages ──
// 这是入站转换的核心：把 Anthropic 的多块消息翻译成 OpenAI 的 role/content/tool_calls/tool_call_id。
//   - assistant 的 tool_use → OpenAI 的 tool_calls
//   - user 的 tool_result → OpenAI 的 role:"tool" + tool_call_id
//   - 文本块 → content 字符串

func buildOpenAIMessages(req anthropicReq) []openAIMessage {
	var out []openAIMessage
	if sys := systemToText(req.System); sys != "" {
		out = append(out, openAIMessage{Role: "system", Content: sys})
	}
	for _, m := range req.Messages {
		text, toolUses, toolResults := parseContent(m.Content)
		if m.Role == "assistant" {
			om := openAIMessage{Role: "assistant"}
			if text != "" {
				om.Content = text
			}
			for _, tu := range toolUses {
				args := string(tu.Input)
				if args == "" {
					args = "{}"
				}
				om.ToolCalls = append(om.ToolCalls, openAIToolCall{
					ID:   tu.ID,
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: tu.Name, Arguments: args},
				})
			}
			out = append(out, om)
		} else { // user
			if text != "" {
				out = append(out, openAIMessage{Role: "user", Content: text})
			}
			for _, tr := range toolResults {
				var c any
				if len(tr.Content) > 0 {
					// tool_result.content 可能是字符串或数组，原样透传
					var cs string
					if json.Unmarshal(tr.Content, &cs) == nil {
						c = cs
					} else {
						c = tr.Content
					}
				}
				out = append(out, openAIMessage{
					Role:       "tool",
					ToolCallID: tr.ToolUseID,
					Content:    c,
				})
			}
		}
	}
	return out
}

// ── Anthropic /v1/messages 主入口 ──

func AnthropicMessages(c *gin.Context) {
	var req anthropicReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"detail": "bad request: " + err.Error()})
		return
	}

	// 模型名映射（同 chat.go）。
	originalModel := req.Model
	req.Model = ResolveModel(req.Model)
	if req.Model != originalModel {
		log.Printf("[ANTHROPIC] %s -> %s", originalModel, req.Model)
	}

	// 入站转换：Anthropic → OpenAI。
	openaiMessages := buildOpenAIMessages(req)
	tools := convertTools(req.Tools)
	toolChoice := convertToolChoice(req.ToolChoice)

	payload := map[string]any{
		"model":      req.Model,
		"messages":   openaiMessages,
		"max_tokens": req.MaxTokens,
		"stream":     req.Stream,
	}
	if tools != nil {
		payload["tools"] = tools
	}
	if toolChoice != nil {
		payload["tool_choice"] = toolChoice
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		payload["stop"] = req.StopSequences
	}

	body, _ := json.Marshal(payload)
	httpReq, _ := http.NewRequest("POST",
		fmt.Sprintf("%s/chat/completions", getEnvURL()),
		bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer sk-1qaz@WSX@123456")
	httpReq.Header.Set("Content-Type", "application/json")

	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
		streamAnthropic(c, httpReq, originalModel)
		return
	}

	// 非流式：向上游要完整响应，转成 Anthropic 格式后返回。
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		c.JSON(502, gin.H{"detail": err.Error()})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		c.Data(resp.StatusCode, "application/json", respBody)
		return
	}
	c.JSON(200, openAIToAnthropic(respBody, originalModel))
}

// ── OpenAI 响应 -> Anthropic 格式 ──
// 把 OpenAI 的 choices[].message（文本 + tool_calls）转成 Anthropic 的 content 数组
// （text 块 + tool_use 块），并映射 token 用量字段名。

func openAIToAnthropic(body []byte, model string) gin.H {
	var o struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content   any              `json:"content"`
				ToolCalls []openAIToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	json.Unmarshal(body, &o)

	var content []gin.H
	if len(o.Choices) > 0 {
		// 文本块
		if s, ok := o.Choices[0].Message.Content.(string); ok && s != "" {
			content = append(content, gin.H{"type": "text", "text": s})
		}
		// 工具调用块（tool_use）
		for _, tc := range o.Choices[0].Message.ToolCalls {
			input := json.RawMessage(tc.Function.Arguments)
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			content = append(content, gin.H{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
			})
		}
	}
	if content == nil {
		content = []gin.H{{"type": "text", "text": ""}}
	}

	return gin.H{
		"id":      "msg_" + shortID(body),
		"type":    "message",
		"role":    "assistant",
		"content": content,
		"model":   model,
		"usage": gin.H{
			"input_tokens":  o.Usage.PromptTokens,
			"output_tokens": o.Usage.CompletionTokens,
		},
	}
}

// shortID 用响应体长度生成一个简短 ID（仅用于 msg_ 前缀的唯一性，非加密用途）。
func shortID(b []byte) string {
	h := fmt.Sprintf("%x", len(b))
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}

// ── 流式：OpenAI SSE -> Anthropic SSE ──
// 把 OpenAI 的 chat/completions 流（data: {...delta...}）翻译成 Anthropic 的 SSE 事件序列：
//   message_start → content_block_start → content_block_delta* → content_block_stop → message_delta → message_stop
// 文本和工具调用分属不同的 content_block（文本 index=0，第 n 个工具 index=n）。

func streamAnthropic(c *gin.Context, httpReq *http.Request, model string) {
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		c.JSON(502, gin.H{"detail": err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		c.Data(resp.StatusCode, "application/json", b)
		return
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.WriteHeader(http.StatusOK)
	flusher, _ := c.Writer.(http.Flusher)

	// write 写一个 SSE 事件：event: <name>\ndata: <json>\n\n
	write := func(event, data string) {
		c.Writer.Write([]byte("event: " + event + "\n"))
		c.Writer.Write([]byte("data: " + data + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	write("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_stream","type":"message","role":"assistant","model":%q,"content":[],"usage":{"input_tokens":0,"output_tokens":0}}}`, model))

	type streamTool struct {
		id, name, args string
		started        bool
	}
	var tools []streamTool
	textStarted := false
	inputTokens, outputTokens := 0, 0

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			ID      string `json:"id"`
			Choices []struct {
				Delta struct {
					Content   *string `json:"content"`
					Role      string  `json:"role"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		// 累计 token 用量（用于最后的 message_delta）。
		if chunk.Usage.PromptTokens > 0 {
			inputTokens = chunk.Usage.PromptTokens
		}
		if chunk.Usage.CompletionTokens > 0 {
			outputTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		// 文本增量
		if delta.Content != nil && *delta.Content != "" {
			if !textStarted {
				write("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
				textStarted = true
			}
			write("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, *delta.Content))
		}

		// 工具调用增量：每个 tool_call index 对应一个 content_block（index+1）。
		for _, tc := range delta.ToolCalls {
			idx := tc.Index
			for len(tools) <= idx {
				tools = append(tools, streamTool{})
			}
			t := &tools[idx]
			if tc.ID != "" {
				t.id = tc.ID
			}
			if tc.Function.Name != "" {
				t.name = tc.Function.Name
			}
			if !t.started {
				// 工具 content_block 起始事件（type=tool_use）。
				write("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q}}`, idx+1, t.id, t.name))
				t.started = true
			}
			if tc.Function.Arguments != "" {
				t.args += tc.Function.Arguments
				write("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%q}}`, idx+1, tc.Function.Arguments))
			}
		}
	}

	// 收尾：先发各 content_block 的 stop，再发 message_delta（带用量）和 message_stop。
	if textStarted {
		write("content_block_stop", `{"type":"content_block_stop","index":0}`)
	}
	for i := range tools {
		write("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i+1))
	}
	write("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":%d,"output_tokens":%d}}`, inputTokens, outputTokens))
	write("message_stop", `{"type":"message_stop"}`)
}
