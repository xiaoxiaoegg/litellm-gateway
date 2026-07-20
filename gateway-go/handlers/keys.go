package handlers

import (
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"token-gateway/database"
	"token-gateway/models"
)

// createKeyReq 是创建 Key 的请求体，只需要一个名字。
type createKeyReq struct {
	KeyName string `json:"key_name" binding:"required"`
}

// ListKeys 列出当前登录用户自己名下的所有 API Key（按创建时间倒序）。
func ListKeys(c *gin.Context) {
	uid, _ := c.Get("user_id")
	var keys []models.UserApiKey
	database.DB.Where("user_id = ?", uid).Order("created_at desc").Find(&keys)
	c.JSON(200, gin.H{"keys": keys})
}

// CreateKey 为当前用户新建一个 API Key。
// 关键安全点：明文只在响应里返回这一次，数据库只存 SHA-256 哈希（ApiKey 字段）。
// KeyPrefix 取明文前 16 位，方便用户在 Key 列表里辨识。
func CreateKey(c *gin.Context) {
	var req createKeyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"detail": "请提供 Key 名称"})
		return
	}
	uid, _ := c.Get("user_id")
	userID, _ := uuid.Parse(uid.(string))

	// 明文 Key 形如 sk-<uuid><uuid>，最长 64 字符；再对明文做 SHA-256 作为数据库存储值。
	raw := fmt.Sprintf("sk-%s%s", uuid.New().String(), uuid.New().String())
	if len(raw) > 64 {
		raw = raw[:64]
	}
	hashed := fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
	prefix := raw[:16]

	key := models.UserApiKey{
		ID:        uuid.New(),
		UserID:    userID,
		KeyName:   req.KeyName,
		ApiKey:    hashed,
		KeyPrefix: prefix,
		IsActive:  true,
	}
	database.DB.Create(&key)

	// 注意：api_key（明文）只在此时返回，务必提示用户保存。
	c.JSON(200, gin.H{
		"id": key.ID.String(), "key_name": key.KeyName,
		"api_key": raw, "key_prefix": key.KeyPrefix, "is_active": key.IsActive,
	})
}

// DeleteKey 删除某个 Key（仅限当前用户自己的，靠 user_id 过滤防越权）。
func DeleteKey(c *gin.Context) {
	uid, _ := c.Get("user_id")
	database.DB.Where("id = ? AND user_id = ?", c.Param("id"), uid).Delete(&models.UserApiKey{})
	c.JSON(200, gin.H{"status": "deleted"})
}

// ToggleKey 启用/禁用某个 Key（取反当前状态）。
func ToggleKey(c *gin.Context) {
	uid, _ := c.Get("user_id")
	var key models.UserApiKey
	if database.DB.Where("id = ? AND user_id = ?", c.Param("id"), uid).First(&key).Error != nil {
		c.JSON(404, gin.H{"detail": "Key 不存在"})
		return
	}
	key.IsActive = !key.IsActive
	database.DB.Save(&key)
	c.JSON(200, gin.H{"id": key.ID.String(), "is_active": key.IsActive})
}

// GetBalance 返回当前用户余额。
func GetBalance(c *gin.Context) {
	uid, _ := c.Get("user_id")
	var user models.User
	database.DB.First(&user, "id = ?", uid)
	c.JSON(200, gin.H{"balance": user.Balance})
}

// ListTransactions 返回当前用户的交易流水（最近 50 条）。
func ListTransactions(c *gin.Context) {
	uid, _ := c.Get("user_id")
	var txns []models.Transaction
	database.DB.Where("user_id = ?", uid).Order("created_at desc").Limit(50).Find(&txns)
	c.JSON(200, gin.H{"transactions": txns, "total": len(txns)})
}

// ListUsage 返回当前用户的用量明细（最近 50 条）。
func ListUsage(c *gin.Context) {
	uid, _ := c.Get("user_id")
	var records []models.UsageRecord
	database.DB.Where("user_id = ?", uid).Order("created_at desc").Limit(50).Find(&records)
	c.JSON(200, gin.H{"usage": records, "total": len(records)})
}

// getNow 仅返回当前时间，留作工具函数（部分 handler 需要时间戳时使用）。
func getNow() time.Time { return time.Now() }
