package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"token-gateway/database"
	"token-gateway/middleware"
	"token-gateway/models"
)

// authReq 是注册/登录共用的请求体。
type authReq struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// Register 处理 POST /api/auth/register：新建普通用户。
// 校验用户名(>=3)/密码(>=6)长度、查重，密码用 bcrypt 哈希存储，最后签发 JWT 返回。
func Register(c *gin.Context) {
	var req authReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"detail": "请提供用户名和密码"})
		return
	}
	if len(req.Username) < 3 || len(req.Password) < 6 {
		c.JSON(400, gin.H{"detail": "用户名至少3位，密码至少6位"})
		return
	}
	var exist models.User
	if database.DB.Where("username = ?", req.Username).First(&exist).Error == nil {
		c.JSON(409, gin.H{"detail": "用户名已存在"})
		return
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	user := models.User{
		ID:           uuid.New(), // 显式生成主键，避免 GORM 把零值 uuid 插成 NULL 触发 not-null 约束
		Username:     req.Username,
		PasswordHash: string(hash),
		Role:         "user",
		IsActive:     true,
	}
	database.DB.Create(&user)
	token, _ := middleware.GenerateToken(user.ID, user.Username, user.Role)
	c.JSON(200, gin.H{
		"access_token": token,
		"user_id":      user.ID.String(),
		"username":     user.Username,
		"role":         user.Role,
	})
}

// Login 处理 POST /api/auth/login：校验用户名密码，成功签发 JWT。
// 密码比较用 bcrypt.CompareHashAndPassword（防止时序攻击，不直接比对明文）。
func Login(c *gin.Context) {
	var req authReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"detail": "请提供用户名和密码"})
		return
	}
	var user models.User
	if database.DB.Where("username = ?", req.Username).First(&user).Error != nil {
		c.JSON(401, gin.H{"detail": "用户名或密码错误"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)) != nil {
		c.JSON(401, gin.H{"detail": "用户名或密码错误"})
		return
	}
	if !user.IsActive {
		c.JSON(403, gin.H{"detail": "账号已被禁用"})
		return
	}
	token, _ := middleware.GenerateToken(user.ID, user.Username, user.Role)
	c.JSON(200, gin.H{
		"access_token": token,
		"user_id":      user.ID.String(),
		"username":     user.Username,
		"role":         user.Role,
	})
}

// GetMe 处理 GET /api/auth/me：返回当前 JWT 对应用户的基本信息。
func GetMe(c *gin.Context) {
	uid, _ := c.Get("user_id")
	var user models.User
	if database.DB.First(&user, "id = ?", uid).Error != nil {
		c.JSON(404, gin.H{"detail": "用户不存在"})
		return
	}
	c.JSON(200, gin.H{
		"id":        user.ID.String(),
		"username":  user.Username,
		"balance":   user.Balance,
		"role":      user.Role,
		"is_active": user.IsActive,
	})
}

// generateUUID 是 uuid.New 的薄封装，留作工具函数（handler 内需要生成 ID 时直接调用）。
func generateUUID() uuid.UUID {
	return uuid.New()
}
