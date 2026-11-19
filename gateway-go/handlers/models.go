package handlers

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// ModelMap 是"客户端请求模型名 → 实际发给上游的模型名"的显式映射表。
//
// 为什么需要它：Claude Code / Codex 各自硬编码了一堆模型名（claude-*, gpt-*, o3...），
// 但本平台实际只接了少量上游供应商。这里把这些名字统一翻译成平台支持的模型，
// 客户端无需改配置就能跑起来。
//
// 约定：
//   - 翻译结果用平台内部模型名（deepseek-chat / glm-4-flash / kimi / ernie-* / qwen-* 等），
//     这些名字在 handlers.chat.go 的 defaultBaseURL / tryUpstreamKeys 里再决定打哪个供应商。
//   - 直接暴露给客户端的别名（kimi / ernie-bot / qwen-max 等）映射成自身，表示"让上游直连该供应商"。
var ModelMap = map[string]string{
	"claude-3-opus-20240229":     "deepseek-chat",
	"claude-3-5-sonnet-20241022": "deepseek-chat",
	"claude-3-5-sonnet-latest":   "deepseek-chat",
	"claude-3-haiku-20240307":    "glm-4-flash",
	"claude-opus-4-8":            "deepseek-chat",
	"claude-sonnet-4-8":          "deepseek-chat",
	"gpt-4o":                     "deepseek-chat",
	"gpt-4o-mini":                "glm-4-flash",
	"gpt-4-turbo":                "deepseek-chat",
	"gpt-3.5-turbo":              "glm-4-flash",
	// 直接暴露 Kimi (Moonshot) 给 Claude Code / Codex 选用
	"kimi":           "kimi",
	"moonshot-v1-8k": "kimi",
	// Codex / OpenAI 常见推理与通用模型名（未显式列出的一律走下方前缀兜底）
	"o4-mini":      "deepseek-chat",
	"o3-mini":      "deepseek-chat",
	"o3":           "deepseek-chat",
	"o1":           "deepseek-chat",
	"o1-mini":      "glm-4-flash",
	"gpt-4.1":      "deepseek-chat",
	"gpt-4.1-mini": "glm-4-flash",
	"gpt-4.1-nano": "glm-4-flash",
	"gpt-5":        "deepseek-chat",
	"gpt-5-mini":   "deepseek-chat",
	"gpt-5-nano":   "glm-4-flash",
	// 千帆（百度）系列：映射成自身，由 chat.go 的 tryUpstreamKeys 直连百度
	"ernie-bot":            "ernie-bot",
	"ernie-bot-turbo":      "ernie-bot-turbo",
	"ernie-4.0-8k":         "ernie-bot",
	"qwen-max":             "qwen-max",
	"qwen-plus":            "qwen-plus",
	"qwen-turbo":           "qwen-plus",
	"qwen2.5-72b-instruct": "qwen-max",
	// 智谱 GLM 系列：显式映射成自身，让客户端能直接指定用 GLM（否则会被下方 glm- 前缀兜底改写到 deepseek-chat）
	"glm-4-flash":      "glm-4-flash",
	"glm-4-flash-fast": "glm-4-flash-fast",
	"glm-4-plus":       "glm-4-flash",
	// Codex 实际会发送的 deepseek 推理模型名（未显式映射时也会走前缀兜底，这里显式列出更清晰）
	"deepseek-v4-pro":   "deepseek-chat",
	"deepseek-reasoner": "deepseek-chat",
	"deepseek-chat":     "deepseek-chat",
}

// AvailableModels 是 /v1/models 端点返回给客户端的模型清单（客户端用来发现"我能用哪些模型"）。
// 注意它只影响客户端"看到"的列表，不限制实际能请求的模型名——未列出的名字仍会走 ResolveModel 兜底。
var AvailableModels = []string{
	"claude-3-5-sonnet-latest",
	"claude-3-haiku-20240307",
	"claude-opus-4-8",
	"claude-sonnet-4-8",
	"gpt-4o",
	"gpt-4o-mini",
	"gpt-4.1",
	"gpt-4.1-mini",
	"gpt-5",
	"gpt-5-mini",
	"o4-mini",
	"o3-mini",
	"o3",
	"o1",
	"deepseek-chat",
	"glm-4-flash",
	"kimi",
	"ernie-bot",
	"ernie-bot-turbo",
	"qwen-max",
	"qwen-plus",
}

// ResolveModel 把客户端请求里的模型名翻译成实际上游模型名。
// 分两步：
//  1. 先查 ModelMap 显式映射；
//  2. 没命中则按前缀兜底：凡是 gpt-/o*/claude-/deepseek-/kimi/moonshot/qwen/ernie/glm- 开头的，
//     一律走 deepseek-chat（保证 Claude Code / Codex 随便报什么模型名都能用），其余原样返回。
//
// 想"放通某个新供应商"，要么往 ModelMap 加显式映射，要么往下方前缀兜底的字符串里加前缀。
func ResolveModel(model string) string {
	if mapped, ok := ModelMap[model]; ok {
		return mapped
	}
	// 兜底：未显式映射的常见前缀默认走 deepseek-chat，让客户端无论请求什么模型名都能通过本中转站。
	if strings.HasPrefix(model, "gpt-") ||
		strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4") ||
		strings.HasPrefix(model, "claude-") ||
		strings.HasPrefix(model, "deepseek-") || strings.HasPrefix(model, "kimi") ||
		strings.HasPrefix(model, "moonshot") || strings.HasPrefix(model, "qwen") ||
		strings.HasPrefix(model, "ernie") || strings.HasPrefix(model, "glm-") {
		return "deepseek-chat"
	}
	return model
}

// modelEntry 是 OpenAI 风格 /v1/models 响应里单条模型的形状。
type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ListModels 处理 GET /v1/models，返回 AvailableModels 列表，供客户端发现可用模型。
// 注意这是"无鉴权"也能访问的端点（在 main.go 里没挂在 APIKeyAuth 组下），只暴露名字不暴露任何敏感信息。
func ListModels(c *gin.Context) {
	data := make([]modelEntry, len(AvailableModels))
	for i, m := range AvailableModels {
		data[i] = modelEntry{ID: m, Object: "model", Created: 1700000000, OwnedBy: "token-gateway"}
	}
	c.JSON(200, gin.H{"object": "list", "data": data})
}
