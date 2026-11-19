package handlers

import (
	"runtime"
	"time"

	"github.com/gin-gonic/gin"
	"token-gateway/database"
	"token-gateway/models"
)

// 本文件实现「超级管理员专属」接口（/api/superadmin/*，SuperAdminOnly 保护）：
//   - ChangeRole：变更某用户角色（user/admin/superadmin）——仅超级管理员可授予/回收管理员权限
//   - SystemInfo：平台运行信息（版本、Go 运行时、各类实体计数），用于"性能监控/系统信息"页
// 这两个都是可落地的真实能力，不依赖未实现的外部系统。

var startedAt = time.Now() // 进程启动时间，用于计算 uptime

type changeRoleReq struct {
	Role string `json:"role" binding:"required"` // user / admin / superadmin
}

// ChangeRole 变更目标用户的角色。只有超级管理员能调用（路由已用 SuperAdminOnly 保护）。
// 安全约束：不允许把"最后一个超级管理员"降级，避免平台失去最高权限入口。
func ChangeRole(c *gin.Context) {
	var req changeRoleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"detail": "参数错误"})
		return
	}
	if req.Role != "user" && req.Role != "admin" && req.Role != "superadmin" {
		c.JSON(400, gin.H{"detail": "角色必须是 user / admin / superadmin"})
		return
	}
	var user models.User
	if database.DB.First(&user, "id = ?", c.Param("id")).Error != nil {
		c.JSON(404, gin.H{"detail": "用户不存在"})
		return
	}
	// 若目标当前是超管、且要被降级，需确保系统仍留有至少一个超管。
	if user.Role == "superadmin" && req.Role != "superadmin" {
		var superCount int64
		database.DB.Model(&models.User{}).Where("role = ?", "superadmin").Count(&superCount)
		if superCount <= 1 {
			c.JSON(400, gin.H{"detail": "至少保留一个超级管理员，无法降级最后一个超管"})
			return
		}
	}
	database.DB.Model(&user).Update("role", req.Role)
	c.JSON(200, gin.H{"id": user.ID.String(), "username": user.Username, "role": req.Role})
}

// SystemInfo 返回平台运行状态与实体计数，供"系统信息 / 性能监控"页展示。
func SystemInfo(c *gin.Context) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	count := func(model interface{}) int64 {
		var n int64
		database.DB.Model(model).Count(&n)
		return n
	}
	var roleUser, roleAdmin, roleSuper int64
	database.DB.Model(&models.User{}).Where("role = ?", "user").Count(&roleUser)
	database.DB.Model(&models.User{}).Where("role = ?", "admin").Count(&roleAdmin)
	database.DB.Model(&models.User{}).Where("role = ?", "superadmin").Count(&roleSuper)

	c.JSON(200, gin.H{
		"version":        "2.0.0-go",
		"go_version":     runtime.Version(),
		"goroutines":     runtime.NumGoroutine(),
		"num_cpu":        runtime.NumCPU(),
		"mem_alloc_mb":   float64(m.Alloc) / 1024 / 1024,
		"uptime_seconds": int64(time.Since(startedAt).Seconds()),
		"counts": gin.H{
			"users":         count(&models.User{}),
			"api_keys":      count(&models.UserApiKey{}),
			"upstream_keys": count(&models.UpstreamKey{}),
			"teams":         count(&models.Team{}),
			"redeem_codes":  count(&models.RedemptionCode{}),
			"usage_records": count(&models.UsageRecord{}),
		},
		"roles": gin.H{
			"user":       roleUser,
			"admin":      roleAdmin,
			"superadmin": roleSuper,
		},
	})
}
