package handlers

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"token-gateway/database"
	"token-gateway/models"
)

// 本文件是管理后台（/api/admin/*）的接口实现，涵盖三类资源：
//   1. 用户管理：列表 / 新建 / 充值 / 扣费 / 设每日配额
//   2. 上游 Key 管理：列表 / 新增 / 删除（按 model_name 匹配，见 chat.go 的 tryUpstreamKeys）
//   3. 模型定价：列表 / 设置（影响 chat.go 的 calculateCost 计费结果）
//
// 这些接口都在 main.go 里被 JWTAuth + AdminOnly 双重保护，只有 admin 角色能访问。

// ── Users ──

type createUserReq struct {
	Username       string  `json:"username" binding:"required"`
	Password       string  `json:"password" binding:"required"`
	Role           string  `json:"role"`
	InitialBalance float64 `json:"initial_balance"`
}

type rechargeReq struct {
	Amount      float64 `json:"amount" binding:"required"`
	Description string  `json:"description"`
}

// ListUsers 返回所有用户，并为每个用户附带 Key 数量与累计花费（来自 UsageRecord）。
func ListUsers(c *gin.Context) {
	var users []models.User
	database.DB.Order("created_at desc").Find(&users)
	type row struct {
		ID         string  `json:"id"`
		Username   string  `json:"username"`
		Balance    float64 `json:"balance"`
		Role       string  `json:"role"`
		IsActive   bool    `json:"is_active"`
		KeyCount   int     `json:"key_count"`
		TotalSpent float64 `json:"total_spent"`
		CreatedAt  string  `json:"created_at"`
	}
	result := make([]row, len(users))
	for i, u := range users {
		var kc int64
		database.DB.Model(&models.UserApiKey{}).Where("user_id = ?", u.ID).Count(&kc)
		var ts float64
		database.DB.Model(&models.UsageRecord{}).Where("user_id = ?", u.ID).Select("COALESCE(SUM(cost),0)").Scan(&ts)
		result[i] = row{
			ID: u.ID.String(), Username: u.Username, Balance: u.Balance,
			Role: u.Role, IsActive: u.IsActive, KeyCount: int(kc),
			TotalSpent: ts, CreatedAt: u.CreatedAt.Format("2006-01-02T15:04:05"),
		}
	}
	c.JSON(200, gin.H{"users": result, "total": len(result)})
}

// CreateUser 新建用户（管理员代建）。默认角色 user，密码 bcrypt 哈希。
// 注意：先显式查重，避免把"其他建库错误"误报成"用户名已存在"（原实现缺陷）。
func CreateUser(c *gin.Context) {
	var req createUserReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"detail": "参数错误"})
		return
	}
	if req.Role == "" {
		req.Role = "user"
	}
	// 显式查重：用户名唯一索引冲突才是"已存在"。
	var exist models.User
	if database.DB.Where("username = ?", req.Username).First(&exist).Error == nil {
		c.JSON(409, gin.H{"detail": "用户名已存在"})
		return
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	user := models.User{
		ID:       uuid.New(), // 必须显式生成主键，否则 GORM 把零值 uuid 插成 NULL 触发 not-null 约束
		Username: req.Username, PasswordHash: string(hash),
		Balance: req.InitialBalance, Role: req.Role, IsActive: true,
	}
	if err := database.DB.Create(&user).Error; err != nil {
		// 兜底：唯一约束冲突仍按 409；其余错误返回 500 并暴露真实原因，便于排查。
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			c.JSON(409, gin.H{"detail": "用户名已存在"})
			return
		}
		c.JSON(500, gin.H{"detail": "创建用户失败: " + err.Error()})
		return
	}
	c.JSON(200, gin.H{"id": user.ID.String(), "username": user.Username, "balance": user.Balance, "role": user.Role})
}

// RechargeUser 给指定用户充值（amount>0）。用 UpdateColumn 直接加余额，并写一条 recharge 交易流水。
func RechargeUser(c *gin.Context) {
	var req rechargeReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Amount <= 0 {
		c.JSON(400, gin.H{"detail": "金额必须大于0"})
		return
	}
	var user models.User
	if database.DB.First(&user, "id = ?", c.Param("id")).Error != nil {
		c.JSON(404, gin.H{"detail": "用户不存在"})
		return
	}
	// 原子加余额（基于查出来的旧值；并发充值极端情况可用 SQL 表达式，这里简单处理）。
	database.DB.Model(&user).UpdateColumn("balance", user.Balance+req.Amount)
	txn := models.Transaction{UserID: user.ID, Amount: req.Amount, Type: "recharge", Description: req.Description}
	database.DB.Create(&txn)
	c.JSON(200, gin.H{"balance": user.Balance + req.Amount})
}

// DeductUser 手动扣费（管理员操作）。余额不足返回 400，不执行扣费。
func DeductUser(c *gin.Context) {
	var req rechargeReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Amount <= 0 {
		c.JSON(400, gin.H{"detail": "金额必须大于0"})
		return
	}
	var user models.User
	if database.DB.First(&user, "id = ?", c.Param("id")).Error != nil {
		c.JSON(404, gin.H{"detail": "用户不存在"})
		return
	}
	if user.Balance < req.Amount {
		c.JSON(400, gin.H{"detail": "余额不足"})
		return
	}
	database.DB.Model(&user).UpdateColumn("balance", user.Balance-req.Amount)
	txn := models.Transaction{UserID: user.ID, Amount: -req.Amount, Type: "deduct", Description: req.Description}
	database.DB.Create(&txn)
	c.JSON(200, gin.H{"balance": user.Balance - req.Amount})
}

type setQuotaReq struct {
	DailyQuota int64 `json:"daily_quota"`
}

// SetUserQuota 设置用户的每日 token 配额（0 表示不限）。具体校验在 chat.go 的 finishChat 里执行。
func SetUserQuota(c *gin.Context) {
	var req setQuotaReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"detail": "参数错误"})
		return
	}
	var user models.User
	if database.DB.First(&user, "id = ?", c.Param("id")).Error != nil {
		c.JSON(404, gin.H{"detail": "用户不存在"})
		return
	}
	database.DB.Model(&user).Update("daily_quota", req.DailyQuota)
	c.JSON(200, gin.H{"id": user.ID.String(), "daily_quota": req.DailyQuota})
}

// ── Upstream Keys ──
// 上游 Key 是直接对接供应商的真实 Key，按 model_name 匹配（见 chat.go tryUpstreamKeys）。

func ListUpstreamKeys(c *gin.Context) {
	var keys []models.UpstreamKey
	database.DB.Order("model_name, priority").Find(&keys)
	c.JSON(200, gin.H{"keys": keys})
}

func AddUpstreamKey(c *gin.Context) {
	var key models.UpstreamKey
	if err := c.ShouldBindJSON(&key); err != nil {
		c.JSON(400, gin.H{"detail": "参数错误"})
		return
	}
	database.DB.Create(&key)
	c.JSON(200, gin.H{"id": key.ID.String(), "status": "created"})
}

func DeleteUpstreamKey(c *gin.Context) {
	database.DB.Delete(&models.UpstreamKey{}, "id = ?", c.Param("id"))
	c.JSON(200, gin.H{"status": "deleted"})
}

// ── Model Pricing ──
// 定价影响 chat.go 的 calculateCost：每千 token 的 prompt/completion 价格。

func ListPricing(c *gin.Context) {
	var pricing []models.ModelPricing
	database.DB.Find(&pricing)
	c.JSON(200, gin.H{"pricing": pricing})
}

// SetPricing 新增或更新某模型的定价（按 model_name 存在与否决定 Create 还是 Update）。
func SetPricing(c *gin.Context) {
	var req models.ModelPricing
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"detail": "参数错误"})
		return
	}
	var existing models.ModelPricing
	if database.DB.Where("model_name = ?", req.ModelName).First(&existing).Error == nil {
		// 已存在则更新单价与币种。
		database.DB.Model(&existing).Updates(map[string]interface{}{
			"prompt_price_per_1k":     req.PromptPricePer1k,
			"completion_price_per_1k": req.CompletionPricePer1k,
			"currency":                req.Currency,
		})
	} else {
		database.DB.Create(&req)
	}
	c.JSON(200, gin.H{"status": "saved", "model_name": req.ModelName})
}
