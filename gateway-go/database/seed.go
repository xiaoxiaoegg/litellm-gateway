package database
// seed.go 负责数据库的初始种子数据写入，确保首次启动时有 admin 账号和默认模型定价。
import (
	"strconv"

	"github.com/google/uuid"
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
	// seed admin：先查是否存在，不存在才创建（管理员账号）。
	var count int64
	DB.Model(&models.User{}).Where("username = ?", "admin").Count(&count)
	if count == 0 {
		hash, _ := bcrypt.GenerateFromPassword([]byte("admin123"), bcrypt.DefaultCost)
		DB.Create(&models.User{
			ID:           uuid.New(), // 显式生成主键，避免零值 uuid 触发 not-null 约束
			Username:     "admin",
			PasswordHash: string(hash),
			Balance:      100000,
			Role:         "admin",
			IsActive:     true,
		})
	}

	// seed superadmin：确保至少存在一个超级管理员账号（幂等）。
	// 超级管理员拥有平台最高权限（角色变更、系统信息等）。默认 superadmin / super123。
	DB.Model(&models.User{}).Where("role = ?", "superadmin").Count(&count)
	if count == 0 {
		hash, _ := bcrypt.GenerateFromPassword([]byte("super123"), bcrypt.DefaultCost)
		DB.Create(&models.User{
			ID:           uuid.New(), // 显式生成主键，避免零值 uuid 触发 not-null 约束
			Username:     "superadmin",
			PasswordHash: string(hash),
			Balance:      100000,
			Role:         "superadmin",
			IsActive:     true,
		})
	}

	// seed model pricing：列出默认支持的模型及其单价。
	// 在这一块可以具体改动对外的模型价格
	// 顺序：模型名 / 每千 prompt token 价格 / 每千 completion token 价格。
	pricing := []struct{ Name, Prompt, Completion string }{
	// DeepSeek
	{"deepseek-chat", "0.00014", "0.00028"},
	{"deepseek-coder", "0.00014", "0.00028"},

	// 智谱 GLM
	{"glm-4-plus", "0.10", "0.10"},
	{"glm-4-flash", "0.001", "0.001"},
	{"glm-4v-plus", "0.10", "0.10"},

	// Kimi (Moonshot)
	{"moonshot-v1-8k", "0.012", "0.012"},
	{"moonshot-v1-32k", "0.024", "0.024"},
	{"moonshot-v1-128k", "0.060", "0.060"},

	// 通义千问
	{"qwen-turbo", "0.0008", "0.0008"},
	{"qwen-plus", "0.004", "0.008"},
	{"qwen-max", "0.02", "0.06"},
	{"qwen-long", "0.0005", "0.001"},

	// 百度千帆
	{"ernie-4.0-turbo", "0.02", "0.02"},
	{"ernie-3.5-turbo", "0.004", "0.004"},
	{"ernie-speed", "0.001", "0.001"},
}
	for _, p := range pricing {
		// 同模型名已有定价则跳过，避免覆盖管理员后来改过的值。
		DB.Model(&models.ModelPricing{}).Where("model_name = ?", p.Name).Count(&count)
		if count == 0 {
			DB.Create(&models.ModelPricing{
				ID:                   uuid.New(), // 显式生成主键，避免零值 uuid 触发 not-null 约束
				ModelName:            p.Name,
				PromptPricePer1k:     parseFloat(p.Prompt),
				CompletionPricePer1k: parseFloat(p.Completion),
				Currency:             "points",
			})
		}
	}
}
