// 包 database 负责数据库连接初始化与建表迁移。
// 依赖 GORM + PostgreSQL，使用 gorm.io/gorm + gorm.io/driver/postgres。
// 网关使用 PostgreSQL + GORM。Init() 在 main 启动时调用一次，
// 它读取数据库连接串、建连接、自动迁移所有模型表，并写入种子数据。
package database

import (
	"fmt"
	"os"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"token-gateway/models"
)

// DB 是全局数据库连接句柄，handler 层通过 database.DB 直接操作数据库。
// 注意：GORM 的会话默认非事务、自动复用连接池，多个请求并发访问是安全的。
var DB *gorm.DB

// Init 初始化数据库：建立连接 + 自动迁移表结构 + 写入种子数据。失败会 panic 终止启动。
func Init() {
	// 连接串优先读环境变量 APP_DATABASE_URL（docker-compose 里注入），缺失则用本地默认值。
	dsn := os.Getenv("APP_DATABASE_URL")
	if dsn == "" {
		dsn = "postgresql://llmproxy:dbpassword9090@db:5432/litellm"
	}

	var err error
	DB, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn), // 只打 Warn 及以上，避免 SQL 日志刷屏
	})
	if err != nil {
		panic(fmt.Sprintf("database connection failed: %v", err))
	}

	// AutoMigrate 会创建表（若不存在）并按结构体字段变更追加列/索引。
	// 它不会删列、不会丢数据，可安全重复执行。
	DB.AutoMigrate(
		&models.User{},
		&models.UserApiKey{},
		&models.UpstreamKey{},
		&models.ModelPricing{},
		&models.UsageRecord{},
		&models.Transaction{},
		&models.Team{},
		&models.RedemptionCode{},
	)

	// 首次启动写入 admin 账号与默认模型定价。
	seedDefaultData()
}
