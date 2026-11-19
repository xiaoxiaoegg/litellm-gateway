// 包 models 定义所有数据库表结构（GORM 模型）。
//
// 这些结构体同时承担两个角色：
//  1. GORM 用它自动建表/迁移（gorm tag 决定列类型、索引、默认值）。
//  2. JSON 序列化时决定对外返回的字段（json tag），带 `json:"-"` 的字段（如密码哈希、真实 Key）不会外泄。
//
// 实体关系：User(1) ──(1:N)──> UserApiKey（一个用户可创建多个 API Key）
//
//	UsageRecord / Transaction 都通过 user_id 关联到 User。
package models

import (
	"time"

	"github.com/google/uuid"
)

// User 是平台账号（普通用户或管理员）。余额 balance 是所有计费动作的扣减目标。
type User struct {
	ID              uuid.UUID    `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	Username        string       `gorm:"uniqueIndex;size:64;not null" json:"username"`
	PasswordHash    string       `gorm:"size:256;not null" json:"-"` // 密码哈希，绝不外泄
	Balance         float64      `gorm:"type:numeric(12,4);default:0" json:"balance"`
	Role            string       `gorm:"size:16;default:user" json:"role"` // "user" / "admin" / "superadmin"
	TeamID          *uuid.UUID   `gorm:"type:uuid;index" json:"team_id"`   // 所属团队（可空）
	IsActive        bool         `gorm:"default:true" json:"is_active"`
	DailyQuota      int64        `gorm:"default:0" json:"daily_quota"`       // 每日 token 上限，0 表示不限
	UsedDailyTokens int64        `gorm:"default:0" json:"used_daily_tokens"` // 已用（按天滚动重置）
	QuotaResetAt    time.Time    `json:"quota_reset_at"`                     // 下次每日配额重置时间
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
	ApiKeys         []UserApiKey `gorm:"foreignKey:UserID" json:"-"` // 关联 Key（查询时用）
}

// UserApiKey 是用户创建的 API Key。数据库只存 SHA-256 哈希（ApiKey 字段），
// 明文只在 CreateKey 时返回一次。KeyPrefix 是明文前 16 位，方便用户在列表里辨认。
type UserApiKey struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	UserID     uuid.UUID  `gorm:"type:uuid;not null;index" json:"user_id"`
	KeyName    string     `gorm:"size:128;not null" json:"key_name"`
	ApiKey     string     `gorm:"uniqueIndex;size:256;not null" json:"api_key,omitempty"` // 存哈希
	KeyPrefix  string     `gorm:"size:16;not null" json:"key_prefix"`                     // 明文前缀，用于展示
	IsActive   bool       `gorm:"default:true" json:"is_active"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"` // 指针：可能从未使用（nil）
}

// UpstreamKey 是"上游供应商的真实 Key"，按 model_name 匹配。
// 客户端请求进来后，若对应模型配置了上游 Key，网关会直接拿它去打供应商（见 handlers.tryUpstreamKeys）。
// Priority 越小越优先；命中 401/403/429 会被自动禁用（避免反复重试失效 Key）。
type UpstreamKey struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	ModelName   string    `gorm:"size:128;not null;index" json:"model_name"`
	ApiKey      string    `gorm:"size:512;not null" json:"-"` // 真实上游 Key，不外泄
	AccountName string    `gorm:"size:128;not null" json:"account_name"`
	BaseURL     string    `gorm:"size:512" json:"base_url"` // 供应商 base url；为空时 handler 里有默认值
	Priority    int       `gorm:"default:0" json:"priority"`
	IsActive    bool      `gorm:"default:true" json:"is_active"`
	TotalQuota  *int64    `json:"total_quota"`                 // 该上游 Key 的总配额（可选，当前仅展示）
	UsedQuota   int64     `gorm:"default:0" json:"used_quota"` // 已用次数
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ModelPricing 是计费单价：每千个 prompt/completion token 的价格。币种默认 points。
// 调用 calculateCost 时按模型名查这张表得出一次请求花费多少。
type ModelPricing struct {
	ID                   uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	ModelName            string    `gorm:"uniqueIndex;size:128;not null" json:"model_name"`
	PromptPricePer1k     float64   `gorm:"type:numeric(10,6);default:0" json:"prompt_price_per_1k"`
	CompletionPricePer1k float64   `gorm:"type:numeric(10,6);default:0" json:"completion_price_per_1k"`
	Currency             string    `gorm:"size:8;default:points" json:"currency"`
	CreatedAt            time.Time `json:"created_at"`
}

// Team 是团队/项目分组，用于把用户按组织归类、隔离资源与预算。
// 当前实现为独立实体：可增删查，用户通过 User.TeamID 关联到某个团队。
type Team struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	Name        string    `gorm:"uniqueIndex;size:64;not null" json:"name"`
	Description string    `gorm:"type:text" json:"description"`
	MonthBudget float64   `gorm:"type:numeric(12,4);default:0" json:"month_budget"` // 团队月度预算（点），0 表示不限
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RedemptionCode 是兑换码/充值码：由管理员批量生成，用户输入后按 Value 充值到余额。
// Status: "active"(未使用) / "used"(已用) / "disabled"(作废)。
type RedemptionCode struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	Code       string     `gorm:"uniqueIndex;size:64;not null" json:"code"`
	Value      float64    `gorm:"type:numeric(12,4);not null" json:"value"` // 面额（点）
	Status     string     `gorm:"size:16;default:active;index" json:"status"`
	UsedBy     *uuid.UUID `gorm:"type:uuid" json:"used_by"` // 使用者 user_id
	UsedByName string     `gorm:"size:64" json:"used_by_name"`
	CreatedAt  time.Time  `json:"created_at"`
	UsedAt     *time.Time `json:"used_at"`
}

// UsageRecord 是每次模型调用的用量/花费明细（计费后写入），统计看板从这里聚合。
type UsageRecord struct {
	ID               uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	UserID           uuid.UUID  `gorm:"type:uuid;not null;index" json:"user_id"`
	ApiKeyID         *uuid.UUID `gorm:"type:uuid" json:"api_key_id"`
	ModelName        string     `gorm:"size:128;not null" json:"model_name"`
	PromptTokens     int        `gorm:"default:0" json:"prompt_tokens"`
	CompletionTokens int        `gorm:"default:0" json:"completion_tokens"`
	TotalTokens      int        `gorm:"default:0" json:"total_tokens"`
	Cost             float64    `gorm:"type:numeric(12,6);default:0" json:"cost"`
	RequestID        string     `gorm:"size:128" json:"request_id"` // 响应里回写的 ID，便于对账
	CreatedAt        time.Time  `json:"created_at"`
}

// Transaction 是账户余额变动流水（充值 recharge / 扣费 deduct）。
// 注意 Amount 为负数表示扣费。统计看板的"总营收"只统计 recharge。
type Transaction struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	UserID      uuid.UUID `gorm:"type:uuid;not null;index" json:"user_id"`
	Amount      float64   `gorm:"type:numeric(12,4);not null" json:"amount"`
	Type        string    `gorm:"size:16;not null" json:"type"` // "recharge" / "deduct"
	Description string    `gorm:"type:text" json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}
