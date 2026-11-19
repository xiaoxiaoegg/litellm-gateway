package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"token-gateway/database"
	"token-gateway/models"
)

// 兑换事务里用到的哨兵错误，用于把不同失败原因映射到不同 HTTP 状态。
var (
	errNotFound = errors.New("code not found")
	errUsed     = errors.New("code already used")
)

// 本文件实现「兑换码 / 充值码」相关接口：
//   - 管理员(admin/superadmin)：批量生成、列表、删除
//   - 普通用户：输入兑换码充值到自己余额（原子核销：一码一用）
//
// 兑换码是自包含的可落地能力：生成后分发给用户，用户兑换即加余额并写一条充值流水。

// randCode 生成形如 "XXXX-XXXX-XXXX-XXXX" 的大写十六进制兑换码（16 字节熵）。
func randCode() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	h := strings.ToUpper(hex.EncodeToString(b)) // 16 个十六进制字符
	return h[0:4] + "-" + h[4:8] + "-" + h[8:12] + "-" + h[12:16]
}

type genCodesReq struct {
	Count int     `json:"count"` // 生成数量（1-500）
	Value float64 `json:"value"` // 每张面额（点）
}

// GenerateCodes 批量生成兑换码（admin/superadmin）。返回明文码列表，请管理员妥善保存分发。
func GenerateCodes(c *gin.Context) {
	var req genCodesReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"detail": "参数错误"})
		return
	}
	if req.Count < 1 || req.Count > 500 {
		c.JSON(400, gin.H{"detail": "数量需在 1-500 之间"})
		return
	}
	if req.Value <= 0 {
		c.JSON(400, gin.H{"detail": "面额必须大于 0"})
		return
	}
	codes := make([]models.RedemptionCode, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		codes = append(codes, models.RedemptionCode{
			ID:     uuid.New(),
			Code:   randCode(),
			Value:  req.Value,
			Status: "active",
		})
	}
	if err := database.DB.Create(&codes).Error; err != nil {
		c.JSON(500, gin.H{"detail": "生成失败: " + err.Error()})
		return
	}
	plain := make([]string, len(codes))
	for i, cd := range codes {
		plain[i] = cd.Code
	}
	c.JSON(200, gin.H{"count": len(codes), "value": req.Value, "codes": plain})
}

// ListCodes 列出所有兑换码（admin/superadmin），最新在前。
func ListCodes(c *gin.Context) {
	var codes []models.RedemptionCode
	database.DB.Order("created_at desc").Limit(500).Find(&codes)
	var active, used int64
	database.DB.Model(&models.RedemptionCode{}).Where("status = ?", "active").Count(&active)
	database.DB.Model(&models.RedemptionCode{}).Where("status = ?", "used").Count(&used)
	c.JSON(200, gin.H{"codes": codes, "active_count": active, "used_count": used})
}

// DeleteCode 删除/作废一张兑换码（admin/superadmin）。
func DeleteCode(c *gin.Context) {
	database.DB.Delete(&models.RedemptionCode{}, "id = ?", c.Param("id"))
	c.JSON(200, gin.H{"status": "deleted"})
}

type redeemReq struct {
	Code string `json:"code" binding:"required"`
}

// RedeemCode 普通用户兑换：校验码有效且未使用 → 加余额 + 写充值流水 + 标记已用。
// 用 GORM 事务 + 行内条件更新保证并发下"一码一用"。
func RedeemCode(c *gin.Context) {
	var req redeemReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"detail": "请输入兑换码"})
		return
	}
	uid, _ := c.Get("user_id")
	userID, err := uuid.Parse(uid.(string))
	if err != nil {
		c.JSON(401, gin.H{"detail": "身份无效"})
		return
	}
	code := strings.ToUpper(strings.TrimSpace(req.Code))

	var user models.User
	if database.DB.First(&user, "id = ?", userID).Error != nil {
		c.JSON(404, gin.H{"detail": "用户不存在"})
		return
	}

	var value float64
	txErr := database.DB.Transaction(func(tx *gorm.DB) error {
		var rc models.RedemptionCode
		if err := tx.Where("code = ?", code).First(&rc).Error; err != nil {
			return errNotFound
		}
		if rc.Status != "active" {
			return errUsed
		}
		// 条件更新：仅当仍为 active 才置为 used（防并发重复核销）。
		now := time.Now()
		res := tx.Model(&models.RedemptionCode{}).
			Where("id = ? AND status = ?", rc.ID, "active").
			Updates(map[string]interface{}{
				"status":       "used",
				"used_by":      userID,
				"used_by_name": user.Username,
				"used_at":      now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errUsed
		}
		// 加余额 + 流水。
		if err := tx.Model(&models.User{}).Where("id = ?", userID).
			UpdateColumn("balance", user.Balance+rc.Value).Error; err != nil {
			return err
		}
		if err := tx.Create(&models.Transaction{
			ID:          uuid.New(), // 显式生成主键，避免零值 uuid 触发 not-null 约束
			UserID:      userID,
			Amount:      rc.Value,
			Type:        "recharge",
			Description: "兑换码充值 " + code,
		}).Error; err != nil {
			return err
		}
		value = rc.Value
		return nil
	})

	switch txErr {
	case nil:
		c.JSON(200, gin.H{"status": "ok", "value": value, "balance": user.Balance + value})
	case errNotFound:
		c.JSON(404, gin.H{"detail": "兑换码不存在"})
	case errUsed:
		c.JSON(409, gin.H{"detail": "兑换码已被使用或已作废"})
	default:
		c.JSON(500, gin.H{"detail": "兑换失败: " + txErr.Error()})
	}
}
