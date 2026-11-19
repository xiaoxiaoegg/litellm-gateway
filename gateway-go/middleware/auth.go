// 包 middleware 提供两类鉴权中间件 + JWT 签发：
//   - JWTAuth：保护管理面 REST API（/api/*），走标准 Authorization: Bearer <JWT>。
//   - AdminOnly：在 JWTAuth 之后，二次校验 role == admin。
//   - APIKeyAuth：保护代理端点（/v1/*），校验客户端带来的 API Key（注意不是 JWT）。
//
// 两者区别：
//
//	管理面用"账号密码登录后拿到的 JWT"（人类/前端用）；
//	代理端点用"用户创建的 API Key"（给 Claude Code / Codex 这类客户端配置用）。
package middleware

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"token-gateway/database"
	"token-gateway/models"
)

// JWTSecret 是签发/校验 JWT 的密钥。生产环境务必通过环境变量 JWT_SECRET 覆盖默认值。
var JWTSecret = []byte(getEnv("JWT_SECRET", "token-gateway-secret-key-change-me"))

// getEnv 读取环境变量，缺失时返回默认值 def。集中放这里方便统一处理配置。
func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// JWTAuth 是 Gin 中间件：校验请求头里的 JWT，并把 user_id / role 注入上下文供后续 handler 使用。
func JWTAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		// 必须是 "Bearer <token>" 形式，否则 401。
		if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
			c.AbortWithStatusJSON(401, gin.H{"detail": "未提供认证令牌"})
			return
		}
		tokenStr := strings.TrimPrefix(auth, "Bearer ")
		// 用同一个 JWTSecret 校验签名与有效期。
		token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
			return JWTSecret, nil
		})
		if err != nil || !token.Valid {
			c.AbortWithStatusJSON(401, gin.H{"detail": "令牌无效或已过期"})
			return
		}
		// 取出声明并写入 Gin 上下文，下游 handler 用 c.Get("user_id") / c.Get("role") 读取。
		claims := token.Claims.(jwt.MapClaims)
		c.Set("user_id", claims["sub"])
		c.Set("role", claims["role"])
		c.Next()
	}
}

// AdminOnly 必须配合 JWTAuth 使用：管理员(admin)或超级管理员(superadmin)才放行，否则 403。
// 超级管理员天然拥有管理员的一切权限（权限递进：user ⊂ admin ⊂ superadmin）。
func AdminOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		role, _ := c.Get("role")
		if role != "admin" && role != "superadmin" {
			c.AbortWithStatusJSON(403, gin.H{"detail": "需要管理员权限"})
			return
		}
		c.Next()
	}
}

// SuperAdminOnly 必须配合 JWTAuth 使用：仅超级管理员(superadmin)才放行，否则 403。
// 用于平台级最高权限接口（角色变更、系统信息等）。
func SuperAdminOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		role, _ := c.Get("role")
		if role != "superadmin" {
			c.AbortWithStatusJSON(403, gin.H{"detail": "需要超级管理员权限"})
			return
		}
		c.Next()
	}
}

// APIKeyAuth 保护代理端点（/v1/chat/completions、/v1/messages、/v1/responses）。
//
// 设计要点：
//  1. 同时兼容两种鉴权头，因为不同客户端习惯不同：
//     - "Authorization: Bearer <key>"（OpenAI / Codex / 通用）
//     - "x-api-key: <key>"（Anthropic / Claude Code 原生只发这个头）
//  2. master key 直通：环境变量 LITELLM_API_KEY（默认 sk-1qaz@WSX@123456）相当于平台超级 Key，
//     绕过用户/余额校验，方便内部调试或管理员直连。
//  3. 普通 Key 在数据库里存的是 SHA-256 哈希（不存明文），这里对传入 Key 做同样哈希后查表，
//     命中且未禁用、所属用户未禁用、余额 > 0 才放行。
//  4. 放行后把 user_id / api_key_id 注入上下文，供计费 handler 扣费。
func APIKeyAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 支持两种鉴权头（见上方设计要点 1）。
		auth := c.GetHeader("Authorization")
		var rawKey string
		if auth != "" && strings.HasPrefix(auth, "Bearer ") {
			rawKey = strings.TrimPrefix(auth, "Bearer ")
		} else {
			rawKey = c.GetHeader("x-api-key")
		}
		if rawKey == "" {
			c.AbortWithStatusJSON(401, gin.H{"detail": "缺少 API Key"})
			return
		}

		// master key bypass（设计要点 2）
		masterKey := os.Getenv("LITELLM_API_KEY")
		if masterKey == "" {
			masterKey = "sk-1qaz@WSX@123456"
		}
		if rawKey == masterKey {
			c.Set("is_master", true)
			c.Next()
			return
		}
		c.Set("is_master", false)

		// 普通 Key：对传入明文做 SHA-256，用哈希值查库（不存明文，见设计要点 3）。
		hashed := fmt.Sprintf("%x", sha256.Sum256([]byte(rawKey)))
		var key models.UserApiKey
		if err := database.DB.Where("api_key = ?", hashed).First(&key).Error; err != nil {
			c.AbortWithStatusJSON(401, gin.H{"detail": "API Key 无效"})
			return
		}
		// Key 本身被禁用则拒绝。
		if !key.IsActive {
			c.AbortWithStatusJSON(401, gin.H{"detail": "API Key 已禁用"})
			return
		}

		// 校验所属用户是否存在且启用。
		var user models.User
		if err := database.DB.First(&user, "id = ?", key.UserID).Error; err != nil || !user.IsActive {
			c.AbortWithStatusJSON(403, gin.H{"detail": "用户不存在或已禁用"})
			return
		}
		// 余额不足直接拒绝（402），避免继续打到上游白嫖。
		if user.Balance <= 0 {
			c.AbortWithStatusJSON(402, gin.H{"detail": "余额不足，请充值"})
			return
		}

		// 更新 Key 的最后使用时间（用于审计/清理）。
		now := time.Now()
		key.LastUsedAt = &now
		database.DB.Save(&key)

		// 注入上下文，下游计费/记录用量时要用。
		c.Set("user_id", key.UserID.String())
		c.Set("api_key_id", key.ID.String())
		c.Next()
	}
}

// GenerateToken 签发一个 24 小时有效的 JWT，载荷里带 user_id / name / role。
// 注册、登录成功后调用，把 token 返回给前端。
func GenerateToken(userID uuid.UUID, username, role string) (string, error) {
	claims := jwt.MapClaims{
		"sub":  userID.String(),
		"name": username,
		"role": role,
		"exp":  time.Now().Add(24 * time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(JWTSecret)
}
