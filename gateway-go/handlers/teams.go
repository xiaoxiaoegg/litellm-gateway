package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"token-gateway/database"
	"token-gateway/models"
)

// 本文件实现「团队 / 分组」管理接口（admin/superadmin）：增、删、查。
// 团队是独立实体，用户通过 User.TeamID 关联；列表会附带每个团队的成员数。

// ListTeams 返回所有团队，并附带成员数量。
func ListTeams(c *gin.Context) {
	var teams []models.Team
	database.DB.Order("created_at desc").Find(&teams)
	type row struct {
		ID          string  `json:"id"`
		Name        string  `json:"name"`
		Description string  `json:"description"`
		MonthBudget float64 `json:"month_budget"`
		MemberCount int     `json:"member_count"`
		CreatedAt   string  `json:"created_at"`
	}
	result := make([]row, len(teams))
	for i, t := range teams {
		var mc int64
		database.DB.Model(&models.User{}).Where("team_id = ?", t.ID).Count(&mc)
		result[i] = row{
			ID: t.ID.String(), Name: t.Name, Description: t.Description,
			MonthBudget: t.MonthBudget, MemberCount: int(mc),
			CreatedAt: t.CreatedAt.Format("2006-01-02T15:04:05"),
		}
	}
	c.JSON(200, gin.H{"teams": result, "total": len(result)})
}

type createTeamReq struct {
	Name        string  `json:"name" binding:"required"`
	Description string  `json:"description"`
	MonthBudget float64 `json:"month_budget"`
}

// CreateTeam 新建团队（名称唯一）。
func CreateTeam(c *gin.Context) {
	var req createTeamReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"detail": "请填写团队名称"})
		return
	}
	var exist models.Team
	if database.DB.Where("name = ?", req.Name).First(&exist).Error == nil {
		c.JSON(409, gin.H{"detail": "团队名称已存在"})
		return
	}
	team := models.Team{
		ID:          uuid.New(),
		Name:        req.Name,
		Description: req.Description,
		MonthBudget: req.MonthBudget,
	}
	if err := database.DB.Create(&team).Error; err != nil {
		c.JSON(500, gin.H{"detail": "创建失败: " + err.Error()})
		return
	}
	c.JSON(200, gin.H{"id": team.ID.String(), "name": team.Name})
}

// DeleteTeam 删除团队，并把该团队成员的 team_id 置空（不删用户）。
func DeleteTeam(c *gin.Context) {
	id := c.Param("id")
	database.DB.Model(&models.User{}).Where("team_id = ?", id).Update("team_id", nil)
	database.DB.Delete(&models.Team{}, "id = ?", id)
	c.JSON(200, gin.H{"status": "deleted"})
}
