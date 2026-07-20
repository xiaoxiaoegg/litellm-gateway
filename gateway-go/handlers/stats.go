package handlers

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"token-gateway/database"
	"token-gateway/models"
)

// StatsResponse 是 GET /api/admin/stats 的返回结构：今日/本周/全局三档聚合数据。
type StatsResponse struct {
	Today struct {
		TotalRequests int64   `json:"total_requests"`
		TotalTokens   int64   `json:"total_tokens"`
		TotalCost     float64 `json:"total_cost"`
	} `json:"today"`
	Week struct {
		TotalRequests int64   `json:"total_requests"`
		TotalTokens   int64   `json:"total_tokens"`
		TotalCost     float64 `json:"total_cost"`
	} `json:"week"`
	TotalUsers   int64   `json:"total_users"`
	ActiveKeys   int64   `json:"active_keys"`
	TotalRevenue float64 `json:"total_revenue"` // 所有充值的累计金额
}

// GetStats 是管理看板的数据接口：从 UsageRecord / User / UserApiKey / Transaction 聚合统计。
//   - 今日/本周按 created_at 时间窗过滤 UsageRecord；
//   - 总用户数、活跃 Key 数、总营收（仅 recharge 类交易求和）。
func GetStats(c *gin.Context) {
	now := time.Now()
	// 今天 0 点（本地时区）。
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	// 本周一 0 点（Weekday 周日=0，这里用 AddDate 回退到本周一）。
	weekStart := todayStart.AddDate(0, 0, -int(todayStart.Weekday()))

	var s StatsResponse
	db := database.DB.Model(&models.UsageRecord{})

	// 今日聚合
	db.Where("created_at >= ?", todayStart).Count(&s.Today.TotalRequests)
	db.Where("created_at >= ?", todayStart).Select("COALESCE(SUM(total_tokens),0)").Scan(&s.Today.TotalTokens)
	db.Where("created_at >= ?", todayStart).Select("COALESCE(SUM(cost),0)").Scan(&s.Today.TotalCost)

	// 本周聚合
	db.Where("created_at >= ?", weekStart).Count(&s.Week.TotalRequests)
	db.Where("created_at >= ?", weekStart).Select("COALESCE(SUM(total_tokens),0)").Scan(&s.Week.TotalTokens)
	db.Where("created_at >= ?", weekStart).Select("COALESCE(SUM(cost),0)").Scan(&s.Week.TotalCost)

	// 全局聚合
	database.DB.Model(&models.User{}).Count(&s.TotalUsers)
	database.DB.Model(&models.UserApiKey{}).Where("is_active = true").Count(&s.ActiveKeys)
	database.DB.Model(&models.Transaction{}).Where("type = 'recharge'").Select("COALESCE(SUM(amount),0)").Scan(&s.TotalRevenue)

	c.JSON(200, s)
}

// DailyStatRow 是单日的聚合结果。
type DailyStatRow struct {
	Date     string  `json:"date"`     // 日期 YYYY-MM-DD
	Requests int64   `json:"requests"` // 当日请求数
	Tokens   int64   `json:"tokens"`   // 当日总 token 数
	Cost     float64 `json:"cost"`     // 当日消耗(积分)
}

// GetDailyStats 是看板时间范围筛选的数据接口：按天聚合最近 N 天的用量。
//   - 查询参数 days(默认7，范围1~365)：统计窗口天数(含今天)。
//   - 返回固定长度数组，缺失的日期补 0，保证折线图 x 轴连续。
//   - 仅管理员可访问(路由挂在 /api/admin 组下)。
func GetDailyStats(c *gin.Context) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "7"))
	if days < 1 || days > 365 {
		days = 7
	}

	now := time.Now()
	loc := now.Location()
	// 窗口起点：今天往前 (days-1) 天，归零到当天 0 点。
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(days - 1))

	// 按天分组聚合 UsageRecord。DATE() 是 Postgres 日期截断函数。
	var aggs []struct {
		Day      string
		Requests int64
		Tokens   int64
		Cost     float64
	}
	database.DB.Model(&models.UsageRecord{}).
		Where("created_at >= ?", start).
		Select("DATE(created_at) as day, COUNT(*) as requests, COALESCE(SUM(total_tokens),0) as tokens, COALESCE(SUM(cost),0) as cost").
		Group("DATE(created_at)").Order("day").Scan(&aggs)

	byDay := make(map[string]DailyStatRow, len(aggs))
	for _, a := range aggs {
		byDay[a.Day] = DailyStatRow{Date: a.Day, Requests: a.Requests, Tokens: a.Tokens, Cost: a.Cost}
	}

	// 补足窗口内每一天(含无数据的日期)，保证折线图 x 轴连续无空洞。
	rows := make([]DailyStatRow, 0, days)
	for i := 0; i < days; i++ {
		key := start.AddDate(0, 0, i).Format("2006-01-02")
		if v, ok := byDay[key]; ok {
			rows = append(rows, v)
		} else {
			rows = append(rows, DailyStatRow{Date: key, Requests: 0, Tokens: 0, Cost: 0})
		}
	}

	c.JSON(200, gin.H{"days": days, "data": rows})
}
