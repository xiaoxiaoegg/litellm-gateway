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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// genID 生成带前缀的短 ID（resp_/msg_/fc_ 等），用作 Responses API 各对象的 id。
func genID(prefix string) string {
	return prefix + uuid.New().String()[:12]
}

// responsesReq 是 OpenAI Responses API 请求的子集（只取 Codex 实际会用到的字段）。
// 与 chat/completions 不同：对话历史不是 messages，而是一个 input（字符串或 item 数组），
// 且工具、推理强度、上文引用等都在顶层。
type responsesReq struct {
	Model              string `json:"model"`
	Input              any    `json:"input"`        // string 或 []item（function_call / function_call_output 等）
	Instructions       string `json:"instructions"` // 等价于 system 提示
	Tools              []any  `json:"tools,omitempty"`
	Stream             bool   `json:"stream"`
	MaxOutputTokens    int    `json:"max_output_tokens,omitempty"` // 对应 chat 的 max_tokens
	Reasoning          any    `json:"reasoning,omitempty"`         // 推理强度 {effort: "low/medium/high"}
	PreviousResponseID string `json:"previous_response_id,omitempty"`
}

// ResponsesHandler 是 POST /v1/responses 主入口（Codex v0.14+ 实际走这条）。
//
// 整体思路：把 Responses API 请求翻译成 OpenAI chat/completions，经 LiteLLM 上游转发，
// 再把 chat 响应翻回 Responses 格式。两步转换：
//   - 入站：convertInputToMessages（input → chat messages）+ convertResponsesTools
//   - 出站：responsesFromChat（非流式）/ handleResponsesStream（流式 SSE）
func ResponsesHandler(c *gin.Context) {
	var req responsesReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": gin.H{"message": "请求格式错误"}})
		return
	}

	// 模型名映射。
	originalModel := req.Model
	req.Model = ResolveModel(req.Model)
	if req.Model != originalModel {
		log.Printf("[MODEL_MAP] %s -> %s", originalModel, req.Model)
	}

	// 入站转换：把 Responses 的 input 翻译成 chat messages；instructions 当作 system。
	msgs, err := convertInputToMessages(req.Input, req.Instructions)
	if err != nil {
		c.JSON(400, gin.H{"error": gin.H{"message": err.Error()}})
		return
	}

	chatReq := completionReq{
		Model:     req.Model,
		Messages:  msgs,
		Stream:    req.Stream,
		MaxTokens: req.MaxOutputTokens,
		Tools:     convertResponsesTools(req.Tools),
	}

	// 组装上游 chat/completions payload。
	payload := map[string]any{
		"model":    chatReq.Model,
		"messages": msgs,
		"stream":   chatReq.Stream,
	}
	if mt := effectiveMaxTokens(chatReq); mt > 0 {
		payload["max_tokens"] = mt
	}
	if len(chatReq.Tools) > 0 {
		payload["tools"] = chatReq.Tools
	}
	// 把 Codex 的 reasoning.effort 透传给支持推理强度的上游。
	if effort := extractReasoningEffort(req.Reasoning); effort != "" {
		payload["reasoning_effort"] = effort
	}

	if chatReq.Stream {
		handleResponsesStream(c, payload, originalModel)
		return
	}

	body, status := postChat(payload)
	if status != 200 {
		c.Data(status, "application/json", body)
		return
	}
	// 计费：非 master 用户按模型扣费并写用量记录（master key 直通免计费）。
	// 必须在把响应返回给客户端之前完成余额/配额校验，否则上游已消耗却没扣费=白嫖。
	var probe completionResp
	json.Unmarshal(body, &probe)
	if st, msg := chargeForUsage(c, req.Model, probe.Usage.PromptTokens, probe.Usage.CompletionTokens); st != 200 {
		c.JSON(st, gin.H{"detail": msg})
		return
	}
	c.Data(200, "application/json", responsesFromChat(body, originalModel))
}

// ── 转换：Responses input -> chat messages ──

// roleOf 根据 item 推断它在 chat 里的角色：
//   - function_call → assistant（模型要调工具）
//   - function_call_output → user（工具返回结果）
//   - 否则用显式 role 字段，再兜底 user
func roleOf(m map[string]any) string {
	if r, ok := m["role"].(string); ok && r != "" {
		return r
	}
	switch m["type"] {
	case "function_call":
		return "assistant"
	case "function_call_output":
		return "user"
	}
	return "user"
}

// extractText 从各种 content 形态里抽出纯文本：nil→空、string→本身、数组→拼接各 text 块。
func extractText(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		parts := []string{}
		for _, p := range v {
			if pm, ok := p.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	if b, err := json.Marshal(content); err == nil {
		return string(b)
	}
	return ""
}

// convertInputToMessages 是入站转换核心：把 Responses 的 input（字符串或 item 数组）
// 翻译成 OpenAI 的 messages 列表。
//   - input 是字符串 → 当成一条 user 消息
//   - 数组 item：
//   - type=function_call → assistant 消息里的 tool_calls（call_id 缺失时自动生成 fc_ ID）
//   - type=function_call_output → role:"tool" 消息，tool_call_id 对应 call_id
//   - 其余 → 按 role 放文本
//   - instructions 非空时插在最前面的 system 消息
//
// 相邻同角色的文本会被合并到同一个 message（用 buf 缓冲），符合 OpenAI 对消息交替的要求。
func convertInputToMessages(input any, instructions string) ([]message, error) {
	msgs := []message{}
	if instructions != "" {
		msgs = append(msgs, message{Role: "system", Content: instructions})
	}

	var items []any
	switch v := input.(type) {
	case string:
		if v != "" {
			items = []any{map[string]any{"role": "user", "content": v}}
		}
	case []any:
		items = v
	case nil:
		// 空输入，不报错
	default:
		return nil, fmt.Errorf("不支持的 input 类型: %T", input)
	}

	// buf 缓冲当前 assistant 消息，便于把多条 function_call / 文本合并到同一 assistant 消息。
	var buf *message
	flush := func() {
		if buf != nil {
			msgs = append(msgs, *buf)
			buf = nil
		}
	}

	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		role := roleOf(m)

		if role == "assistant" {
			if buf == nil {
				b := message{Role: "assistant"}
				buf = &b
			}
			if m["type"] == "function_call" {
				name, _ := m["name"].(string)
				args, _ := m["arguments"].(string)
				callID, _ := m["call_id"].(string)
				if callID == "" {
					callID = genID("fc_")
				}
				buf.ToolCalls = append(buf.ToolCalls, toolCall{
					ID:   callID,
					Type: "function",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: name, Arguments: args},
				})
			} else {
				text := extractText(m["content"])
				if s, ok := buf.Content.(string); ok && s != "" {
					buf.Content = s + "\n" + text
				} else {
					buf.Content = text
				}
			}
			continue
		}

		flush()
		if m["type"] == "function_call_output" {
			callID, _ := m["call_id"].(string)
			out := extractText(m["output"])
			msgs = append(msgs, message{Role: "tool", Content: out, ToolCallID: callID})
			continue
		}
		msgs = append(msgs, message{Role: role, Content: extractText(m["content"])})
	}
	flush()
	return msgs, nil
}

// convertResponsesTools 把 Responses 的工具定义转成 chat 的 {type:"function", function:{...}} 形式。
//
// 重要：只转发"真正的 function 类型且 name 非空"的工具。原因：
//   - code_interpreter / reasoning 等非函数工具，deepseek 等上游不支持会报错；
//   - 若 name 为空，上游会返回 400（tools[].function.name 不能为空），所以直接丢弃。
func convertResponsesTools(tools []any) []any {
	out := []any{}
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := m["type"].(string); typ != "function" {
			continue
		}
		var name, desc string
		var params any
		// Responses 工具可能直接是 {type,function:{name,description,parameters}}，
		// 也可能是扁平的 {type,name,description,parameters}，两种都兼容。
		if fn, ok := m["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
			desc, _ = fn["description"].(string)
			params = fn["parameters"]
		} else {
			name, _ = m["name"].(string)
			desc, _ = m["description"].(string)
			params = m["parameters"]
		}
		if name == "" {
			continue
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": desc,
				"parameters":  params,
			},
		})
	}
	return out
}

// extractReasoningEffort 从 reasoning 字段取推理强度字符串（low/medium/high），其余返回空。
func extractReasoningEffort(r any) string {
	if m, ok := r.(map[string]any); ok {
		if e, ok := m["effort"].(string); ok {
			return e
		}
	}
	return ""
}

// ── 上游调用 ──

// postChat 用内部 master key 调上游 chat/completions，返回 (body, statusCode)。
func postChat(payload map[string]any) ([]byte, int) {
	b, _ := json.Marshal(payload)
	httpReq, _ := http.NewRequest("POST", fmt.Sprintf("%s/chat/completions", getEnvURL()), bytes.NewReader(b))
	httpReq.Header.Set("Authorization", "Bearer sk-1qaz@WSX@123456")
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, 502
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode
}

// ── 转换：chat completion -> Responses ──

// responseObject 构造 Responses API 的标准响应外壳（含 usage、状态、工具选项）。
func responseObject(id, model, status string, output []any, usage map[string]any) map[string]any {
	if usage == nil {
		usage = map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}
	return map[string]any{
		"id":                  id,
		"object":              "response",
		"created_at":          time.Now().Unix(),
		"model":               model,
		"status":              status,
		"output":              output,
		"usage":               usage,
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
	}
}

// responsesFromChat 把上游 chat 响应翻回 Responses 格式（非流式）。
//   - chat 的 content 文本 → Responses 的 message 块（type=message, content=[{type:output_text,...}]）
//   - chat 的 tool_calls → Responses 的 function_call 块（带 name/arguments/call_id）
//   - usage 字段名从 prompt/completion_tokens 映射到 input/output_tokens
func responsesFromChat(body []byte, originalModel string) []byte {
	var ch completionResp
	json.Unmarshal(body, &ch)

	respID := genID("resp_")
	output := []any{}
	text := ""
	if len(ch.Choices) > 0 {
		msg := ch.Choices[0].Message
		text = extractText(msg.Content)
		if text != "" {
			output = append(output, map[string]any{
				"type": "message", "id": genID("msg_"), "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
			})
		}
		for _, tc := range msg.ToolCalls {
			output = append(output, map[string]any{
				"type": "function_call", "name": tc.Function.Name,
				"arguments": tc.Function.Arguments, "call_id": tc.ID,
			})
		}
	}
	usage := map[string]any{
		"input_tokens":  ch.Usage.PromptTokens,
		"output_tokens": ch.Usage.CompletionTokens,
		"total_tokens":  ch.Usage.TotalTokens,
	}
	out := responseObject(respID, originalModel, "completed", output, usage)
	out["text"] = text // 顶层 text 方便客户端快速取正文
	respBytes, _ := json.Marshal(out)
	return respBytes
}

// ── 流式：chat/completions SSE -> Responses SSE ──
// 把上游 OpenAI 流翻译成 Responses API 的事件流：
//   response.created → response.in_progress →（每个输出项）output_item.added → content_part.added →
//   output_text.delta / function_call_arguments.delta → content_part.done → output_item.done →
//   response.completed
// 文本和多个工具调用会被组织成不同的 output item（各自有 index）。

func handleResponsesStream(c *gin.Context, payload map[string]any, originalModel string) {
	b, _ := json.Marshal(payload)
	httpReq, _ := http.NewRequest("POST", fmt.Sprintf("%s/chat/completions", getEnvURL()), bytes.NewReader(b))
	httpReq.Header.Set("Authorization", "Bearer sk-1qaz@WSX@123456")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		c.JSON(502, gin.H{"error": gin.H{"message": fmt.Sprintf("upstream: %v", err)}})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		c.Data(resp.StatusCode, "application/json", body)
		return
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.WriteHeader(http.StatusOK)

	flusher, _ := c.Writer.(http.Flusher)
	respID := genID("resp_")

	// writeEvt 写一条 Responses SSE 事件：event: <type>\ndata: <json 含 type 字段>\n\n
	writeEvt := func(event string, data map[string]any) {
		data["type"] = event
		j, _ := json.Marshal(data)
		c.Writer.Write([]byte("event: " + event + "\n"))
		c.Writer.Write([]byte("data: " + string(j) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	// tacc 记录一个正在流式累积的工具调用。
	type tacc struct {
		id      string
		name    string
		args    string
		outIdx  int
		started bool
	}

	var (
		itemType    string // 当前正在累积的输出项类型："message" 或 "function_call"
		itemID      string // 当前项的 id
		textAccum   string // 累积的文本
		msgOutIdx   int    // 文本项的 output_index
		toolAcc     []tacc // 各工具调用累积器（按 delta 的 index）
		outIdx      int    // 下一个 output_index 计数器
		outputItems []any  // 已完成的输出项（用于最终 response.completed）
	)

	nextIdx := func() int {
		i := outIdx
		outIdx++
		return i
	}

	// finalize 把"当前正在累积的项"收尾并发对应的 done 事件，然后清空 itemType。
	finalize := func() {
		if itemType == "message" {
			writeEvt("response.content_part.done", map[string]any{
				"item_id": itemID, "output_index": msgOutIdx, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": textAccum, "annotations": []any{}},
			})
			item := map[string]any{
				"type": "message", "id": itemID, "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": textAccum, "annotations": []any{}}},
			}
			writeEvt("response.output_item.done", map[string]any{"output_index": msgOutIdx, "item": item})
			outputItems = append(outputItems, item)
			itemType = ""
		} else if itemType == "function_call" {
			for _, tc := range toolAcc {
				item := map[string]any{"type": "function_call", "name": tc.name, "arguments": tc.args, "call_id": tc.id}
				writeEvt("response.output_item.done", map[string]any{"output_index": tc.outIdx, "item": item})
				outputItems = append(outputItems, item)
			}
			toolAcc = nil
			itemType = ""
		}
	}

	// 开头两个事件。
	writeEvt("response.created", map[string]any{"response": responseObject(respID, originalModel, "in_progress", []any{}, nil)})
	writeEvt("response.in_progress", map[string]any{"response": responseObject(respID, originalModel, "in_progress", []any{}, nil)})

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var usage map[string]any
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
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			usage = map[string]any{
				"input_tokens":  chunk.Usage.PromptTokens,
				"output_tokens": chunk.Usage.CompletionTokens,
				"total_tokens":  chunk.Usage.TotalTokens,
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]

		// 文本增量：若当前项不是 message，先 finalize 上一个项，再开启新的 message 项。
		if ch.Delta.Content != "" {
			if itemType != "message" {
				finalize()
				itemType = "message"
				itemID = genID("msg_")
				msgOutIdx = nextIdx()
				writeEvt("response.output_item.added", map[string]any{
					"output_index": msgOutIdx,
					"item": map[string]any{
						"type": "message", "id": itemID, "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
					},
				})
				writeEvt("response.content_part.added", map[string]any{
					"item_id": itemID, "output_index": msgOutIdx, "content_index": 0,
					"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
				})
			}
			writeEvt("response.output_text.delta", map[string]any{
				"item_id": itemID, "output_index": msgOutIdx, "content_index": 0, "delta": ch.Delta.Content,
			})
			textAccum += ch.Delta.Content
		}

		// 工具调用增量：每个 index 对应一个 function_call 项，参数分片用 function_call_arguments.delta 流式下发。
		for _, tc := range ch.Delta.ToolCalls {
			for len(toolAcc) <= tc.Index {
				toolAcc = append(toolAcc, tacc{})
			}
			if !toolAcc[tc.Index].started {
				finalize()
				itemType = "function_call"
				id := tc.ID
				if id == "" {
					id = genID("fc_") // 上游没给 id 时自己造一个 fc_ ID
				}
				itemID = id
				toolAcc[tc.Index].id = id
				toolAcc[tc.Index].started = true
				toolAcc[tc.Index].outIdx = nextIdx()
				writeEvt("response.output_item.added", map[string]any{
					"output_index": toolAcc[tc.Index].outIdx,
					"item": map[string]any{
						"type": "function_call", "id": id, "name": tc.Function.Name,
						"arguments": "", "call_id": id,
					},
				})
			}
			if tc.Function.Name != "" {
				toolAcc[tc.Index].name += tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				toolAcc[tc.Index].args += tc.Function.Arguments
				writeEvt("response.function_call_arguments.delta", map[string]any{
					"item_id": itemID, "output_index": toolAcc[tc.Index].outIdx,
					"delta": tc.Function.Arguments, "name": toolAcc[tc.Index].name,
				})
			}
		}

		// 上游宣布本项结束（finish_reason 非空）时收尾当前项。
		if ch.FinishReason != "" {
			finalize()
		}
	}
	// 流结束前若还有未收尾的项，补 finalize。
	if itemType != "" {
		finalize()
	}
	if usage == nil {
		usage = map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}
	// 最后发 response.completed（携带完整的 output 列表与用量）。
	writeEvt("response.completed", map[string]any{
		"response": responseObject(respID, originalModel, "completed", outputItems, usage),
	})

	// 计费：流式无法中途拦截，这里在流结束后对非 master 用户记账（扣费 + 写用量）。
	// 即使余额不足也已把结果返回了客户端（边缘情况）；正常情况会正确扣费并留记录，
	// 与 /v1/chat/completions 的计费逻辑（chargeForUsage）保持一致。
	if usage != nil {
		toInt := func(v any) int {
			if f, ok := v.(float64); ok {
				return int(f)
			}
			return 0
		}
		if mdl, ok := payload["model"].(string); ok {
			chargeForUsage(c, mdl, toInt(usage["input_tokens"]), toInt(usage["output_tokens"]))
		}
	}
}
