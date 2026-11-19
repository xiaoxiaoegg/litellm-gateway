// 包 main 是 token-gateway 网关的程序入口。
//
// 整体职责：把"用户侧各种客户端协议"统一转成上游 OpenAI 兼容格式，
// 经 LiteLLM 代理转发到真实的模型供应商（DeepSeek / 通义 / 千帆 / Kimi 等），
// 同时做鉴权、配额、计费、以及给 Prometheus 暴露指标。
//
// 路由大致分四块（详见下方 r.Group 定义）：
//  1. 管理面 REST API（/api/*）：用户注册登录、Key 管理、余额、用量、管理后台。
//  2. Prometheus 指标（/metrics）：无需鉴权，供 Prometheus 抓取。
//  3. 模型列表（/v1/models）：让客户端发现可用模型名。
//  4. 代理端点（/v1/chat/completions、/v1/messages、/v1/responses）：真正转发模型请求，
//     这三条挂在 APIKeyAuth + MetricsMiddleware 中间件组里。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"token-gateway/database"
	"token-gateway/handlers"
	"token-gateway/middleware"
)

func main() {
	// 初始化数据库连接并执行自动迁移（AutoMigrate）+ 种子数据（admin/定价）。
	database.Init()

	// gin 默认引擎（含 Logger 与 Recovery 中间件）。
	r := gin.Default()

	// 健康检查：探针/负载均衡/部署平台用它判断服务是否存活。
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok", "version": "2.0.0-go"})
	})

	// ── 认证路由组：/api/auth ──
	// 注册/登录不需要带 Token；/me 需要 JWT。
	auth := r.Group("/api/auth")
	{
		auth.POST("/register", handlers.Register)
		auth.POST("/login", handlers.Login)
		auth.GET("/me", middleware.JWTAuth(), handlers.GetMe)
	}

	// ── 用户路由组：/api/* ──
	// 所有接口都需要 JWT 鉴权（JWTAuth 会从 Authorization: Bearer <token> 解析出 user_id/role）。
	user := r.Group("/api")
	user.Use(middleware.JWTAuth())
	{
		user.GET("/keys", handlers.ListKeys)                 // 列出当前用户自己的 Key
		user.POST("/keys", handlers.CreateKey)               // 新建一个 API Key（返回明文，只此一次）
		user.DELETE("/keys/:id", handlers.DeleteKey)         // 删除某个 Key
		user.PATCH("/keys/:id/toggle", handlers.ToggleKey)   // 启用/禁用某个 Key
		user.GET("/balance", handlers.GetBalance)            // 查余额
		user.GET("/transactions", handlers.ListTransactions) // 交易流水
		user.GET("/usage", handlers.ListUsage)               // 用量明细
		user.POST("/redeem", handlers.RedeemCode)            // 兑换码充值（普通用户）
	}

	// ── 管理后台路由组：/api/admin ──
	// 需要 JWT 且角色必须为 admin（AdminOnly 拦截非管理员）。
	admin := r.Group("/api/admin")
	admin.Use(middleware.JWTAuth(), middleware.AdminOnly())
	{
		admin.GET("/users", handlers.ListUsers)                  // 用户列表（含 Key 数/累计花费）
		admin.POST("/users", handlers.CreateUser)                // 新建用户
		admin.POST("/users/:id/recharge", handlers.RechargeUser) // 充值
		admin.POST("/users/:id/deduct", handlers.DeductUser)     // 扣费
		admin.POST("/users/:id/quota", handlers.SetUserQuota)    // 设置每日配额

		admin.GET("/upstream-keys", handlers.ListUpstreamKeys)         // 上游供应商 Key 列表
		admin.POST("/upstream-keys", handlers.AddUpstreamKey)          // 新增上游 Key（按模型名匹配）
		admin.DELETE("/upstream-keys/:id", handlers.DeleteUpstreamKey) // 删除上游 Key

		admin.GET("/pricing", handlers.ListPricing) // 模型定价列表
		admin.POST("/pricing", handlers.SetPricing) // 新增/更新某模型定价

		admin.GET("/stats", handlers.GetStats)                   // 今日/本周/全局统计看板数据
		admin.GET("/stats/daily", handlers.GetDailyStats)        // 按天聚合(支持 days 查询参数，供看板时间范围筛选)

		// 团队管理（admin/superadmin）
		admin.GET("/teams", handlers.ListTeams)          // 团队列表（含成员数）
		admin.POST("/teams", handlers.CreateTeam)        // 新建团队
		admin.DELETE("/teams/:id", handlers.DeleteTeam)  // 删除团队

		// 兑换码管理（admin/superadmin）
		admin.GET("/redemption", handlers.ListCodes)            // 兑换码列表
		admin.POST("/redemption", handlers.GenerateCodes)       // 批量生成兑换码
		admin.DELETE("/redemption/:id", handlers.DeleteCode)    // 删除/作废兑换码
	}

	// ── 超级管理员路由组：/api/superadmin ──
	// 需要 JWT 且角色必须为 superadmin（SuperAdminOnly 拦截 admin 与普通用户）。
	sa := r.Group("/api/superadmin")
	sa.Use(middleware.JWTAuth(), middleware.SuperAdminOnly())
	{
		sa.POST("/users/:id/role", handlers.ChangeRole) // 变更用户角色（授予/回收管理员）
		sa.GET("/system", handlers.SystemInfo)          // 平台运行信息 / 性能监控
	}

	// Prometheus 指标端点：故意不加鉴权，因为 Prometheus 抓取时不会带我们的 API Key。
	r.GET("/metrics", handlers.Metrics)

	// 模型发现：客户端（Claude Code / Codex）启动时通常会请求这个端点来确认可用模型。
	r.GET("/v1/models", handlers.ListModels)

	// ── 代理端点组 ──
	// 这三条是"真正消费 token 的路径"，因此挂两个中间件：
	//   - APIKeyAuth：校验客户端带来的 API Key（支持 Bearer 或 x-api-key 两种头）。
	//   - MetricsMiddleware：记录 客户端/模型/状态码/时延 供 /metrics 输出。
	proxy := r.Group("")
	proxy.Use(middleware.APIKeyAuth(), handlers.MetricsMiddleware())
	{
		proxy.POST("/v1/chat/completions", handlers.ChatCompletion) // OpenAI 协议（Codex 传统路径）
		proxy.POST("/v1/messages", handlers.AnthropicMessages)      // Anthropic 协议（Claude Code 原生）
		proxy.POST("/v1/responses", handlers.ResponsesHandler)      // OpenAI Responses API（Codex v0.14+ 实际使用的路径）
	}

	// 静态前端：兜底路由，把未匹配的请求当作静态文件服务（前端管理页面放在 /static）。
	r.NoRoute(func(c *gin.Context) {
		// 对 HTML 页面（含根路径 /）禁用浏览器缓存，避免缓存到旧版
		// （旧版引用被墙的 CDN，会导致 Alpine/Tailwind 加载失败、页面白屏）。
		// vendor 下的 .js 资源带文件名版本，可正常缓存。
		p := c.Request.URL.Path
		if p == "/" || strings.HasSuffix(p, ".html") {
			c.Header("Cache-Control", "no-store")
			c.Header("Pragma", "no-cache")
		}
		http.FileServer(http.Dir("/static")).ServeHTTP(c.Writer, c.Request)
	})

	// 启动 HTTP 服务（监听 8000），用 goroutine 异步跑，主协程下面负责优雅关闭。
	srv := &http.Server{Addr: ":8000", Handler: r}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	// 捕获 SIGINT/SIGTERM，收到后优雅关闭（最多等 5 秒让在途请求完成）。
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
