package database

import (
	"strconv"

	"golang.org/x/crypto/bcrypt"
	"token-gateway/models"
)

// parseFloat 把字符串价格（如 "0.20"）转成 float64，解析失败返回 0。
// 种子定价用字符串写，便于阅读，这里统一转换。
func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// seedDefaultData 在数据库为空时写入初始数据：
//  1. 一个 admin 账号（用户名 admin / 密码 admin123 / 余额 100000 点）。
//  2. 一批模型定价（每千 token 的 prompt / completion 价格，单位 points）。
//
// 仅在记录不存在时写入（幂等），所以每次启动调用都是安全的。
func seedDefaultData() {
	// seed admin：先查是否存在，不存在才创建。
	var count int64
	DB.Model(&models.User{}).Where("username = ?", "admin").Count(&count)
	if count == 0 {
		hash, _ := bcrypt.GenerateFromPassword([]byte("admin123"), bcrypt.DefaultCost)
		DB.Create(&models.User{
			Username:     "admin",
			PasswordHash: string(hash),
			Balance:      100000,
			Role:         "admin",
			IsActive:     true,
		})
	}

	// seed model pricing：列出默认支持的模型及其单价。
	// 顺序：模型名 / 每千 prompt token 价格 / 每千 completion token 价格。
	pricing := []struct{ Name, Prompt, Completion string }{
		{"glm-4-flash", "0.15", "0.60"},
		{"glm-4-flash-fast", "0.10", "0.40"},
		{"deepseek-chat", "0.20", "0.80"},
		{"kimi", "0.25", "1.00"},
		{"ernie-bot", "0.20", "0.80"},
		{"ernie-bot-turbo", "0.10", "0.40"},
		{"qwen-max", "0.40", "1.20"},
		{"qwen-plus", "0.20", "0.80"},
		{"qwen-turbo", "0.08", "0.32"},
	}
	for _, p := range pricing {
		// 同模型名已有定价则跳过，避免覆盖管理员后来改过的值。
		DB.Model(&models.ModelPricing{}).Where("model_name = ?", p.Name).Count(&count)
		if count == 0 {
			DB.Create(&models.ModelPricing{
				ModelName:            p.Name,
				PromptPricePer1k:     parseFloat(p.Prompt),
				CompletionPricePer1k: parseFloat(p.Completion),
				Currency:             "points",
			})
		}
	}
}
