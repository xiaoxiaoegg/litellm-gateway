package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// 本文件实现 Prometheus 指标采集与暴露（零依赖，手写 Prometheus 文本格式）。
//
// 设计：用一个进程内的内存计数器（按 客户端/模型/状态码 三维聚合）记录请求数与累计时延。
// 注意：这是内存态，容器重建会清零；不适合长期留存，长期留存应接 Prometheus + 存储。
//
// 维度说明（metricKey）：
//   - client：claude_code（/v1/messages）/ codex（/v1/chat/completions 与 /v1/responses）/ other
//   - model：请求里的 model 字段（原始客户端模型名，方便看各客户端用量）
//   - status：HTTP 状态码字符串

// metricKey 指标维度：客户端、模型、状态码。
type metricKey struct {
	client string
	model  string
	status string
}

var (
	metricsMu         sync.Mutex                // 保护下方两个 map 的并发读写
	reqCounts         = map[metricKey]uint64{}  // 各维度请求数
	reqLatencyMsTotal = map[metricKey]float64{} // 各维度累计时延（毫秒）
)

// clientFromPath 按路由路径区分客户端类型。
// 这是 Grafana 面板里"按客户端拆分调用量"的依据：Claude Code 走 /v1/messages，Codex 走另外两条。
func clientFromPath(path string) string {
	switch path {
	case "/v1/messages":
		return "claude_code"
	case "/v1/chat/completions", "/v1/responses":
		return "codex"
	default:
		return "other"
	}
}

// MetricsMiddleware 是代理端点的中间件：为每个请求记录 客户端/模型/状态码/时延。
//
// 关键点：为了拿到 model 标签，它会先把请求体读出来反序列化取 model 字段，
// 然后用 io.NopCloser 把 body 还原回去，确保下游 handler（ChatCompletion 等）还能正常解析。
// 时延 = 从进入中间件到 c.Next() 返回（即整个请求处理完成）的耗时。
func MetricsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		client := clientFromPath(c.Request.URL.Path)

		var model string
		if c.Request.Body != nil {
			bodyBytes, err := io.ReadAll(c.Request.Body)
			if err == nil {
				var probe struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(bodyBytes, &probe) == nil {
					model = probe.Model
				}
				// 还原 body，供下游 handler 再次读取。
				c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			}
		}

		c.Next() // 执行后续 handler

		status := strconv.Itoa(c.Writer.Status())
		latencyMs := float64(time.Since(start).Microseconds()) / 1000.0

		// 加锁更新计数器。
		metricsMu.Lock()
		k := metricKey{client, model, status}
		reqCounts[k]++
		reqLatencyMsTotal[k] += latencyMs
		metricsMu.Unlock()
	}
}

// Metrics 处理 GET /metrics，输出 Prometheus 文本格式。Prometheus 服务端会定期抓取这个端点。
// 暴露两个指标：
//   - gateway_requests_total{client,model,status}：请求计数
//   - gateway_request_latency_ms_total{client,model,status}：累计时延（毫秒）
func Metrics(c *gin.Context) {
	metricsMu.Lock()
	defer metricsMu.Unlock()

	var b bytes.Buffer
	b.WriteString("# HELP gateway_requests_total Total proxy requests by client/model/status.\n")
	b.WriteString("# TYPE gateway_requests_total counter\n")
	for k, v := range reqCounts {
		b.WriteString(metricLine("gateway_requests_total", k, strconv.FormatUint(v, 10)))
	}
	b.WriteString("# HELP gateway_request_latency_ms_total Total request latency in milliseconds by client/model/status.\n")
	b.WriteString("# TYPE gateway_request_latency_ms_total counter\n")
	for k, v := range reqLatencyMsTotal {
		b.WriteString(metricLine("gateway_request_latency_ms_total", k, strconv.FormatFloat(v, 'f', 2, 64)))
	}
	c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", b.Bytes())
}

// metricLine 生成一行 Prometheus 样本：name{label=...} value
func metricLine(name string, k metricKey, value string) string {
	return fmt.Sprintf("%s{client=%q,model=%q,status=%q} %s\n",
		name, k.client, k.model, k.status, value)
}
