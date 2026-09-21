package model

import (
	"errors"

	"github.com/pai801/myapi/common/helper"
)

// Group 表示一个分组，每个分组有一个 float64 倍率作为全局 ModelRatio 的乘数。
// default 组倍率为 1.0（完全向后兼容）。
//
// ModelRatio 的类型**必须避免写成带逗号的类型串**（如 type:decimal(10,4)）：
// gorm.io/driver/sqlite 的 AlterColumn 会在第一个逗号处截断列定义，生成多出一个右括号的
// DDL，导致 parseDDL 报 "invalid DDL, unbalanced brackets"，SQLite 部署第二次启动即 FATAL。
// 用 precision/scale 代替 type：SQLite 侧 Float 恒为 real（comma-free），
// MySQL 侧仍是 decimal(10, 4)，语义不变。
// default 写成 1 而不是 1.0：gorm 对 float 字段会用 DefaultValueInterface 渲染，
// 1.0 会被渲染成 "1"，与 tag 里的 "1.0" 不相等，从而每次启动都触发一次多余的 AlterColumn。
type Group struct {
	Id          int     `json:"id"`
	Name        string  `json:"name" gorm:"type:varchar(32);uniqueIndex"`
	ModelRatio  float64 `json:"model_ratio" gorm:"precision:10;scale:4;default:1"`
	CreatedTime int64   `json:"created_time" gorm:"bigint"`
}

func (g *Group) TableName() string {
	return "groups"
}

func GetGroupByName(name string) (*Group, error) {
	if name == "" {
		return nil, errors.New("group name is empty")
	}
	group := Group{}
	err := DB.Where("name = ?", name).First(&group).Error
	if err != nil {
		return nil, err
	}
	return &group, nil
}

func GetGroupById(id int) (*Group, error) {
	if id == 0 {
		return nil, errors.New("group id is empty")
	}
	group := Group{}
	err := DB.First(&group, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &group, nil
}

func GetAllGroups() ([]*Group, error) {
	var groups []*Group
	err := DB.Order("id asc").Find(&groups).Error
	return groups, err
}

func GetGroupList(page int, perPage int) (groups []*Group, total int64, err error) {
	if page < 0 {
		page = 0
	}
	if perPage <= 0 {
		perPage = 10
	}
	offset := page * perPage
	err = DB.Model(&Group{}).Count(&total).Error
	if err != nil {
		return nil, 0, err
	}
	err = DB.Order("id asc").Limit(perPage).Offset(offset).Find(&groups).Error
	return groups, total, err
}

func AddGroup(group *Group) error {
	if group.CreatedTime == 0 {
		group.CreatedTime = helper.GetTimestamp()
	}
	err := DB.Create(group).Error
	return err
}

func UpdateGroup(group *Group) error {
	err := DB.Model(group).Select("name", "model_ratio").Updates(group).Error
	return err
}

func DeleteGroup(id int) error {
	if id == 0 {
		return errors.New("group id is empty")
	}
	group := Group{Id: id}
	err := DB.First(&group, "id = ?", id).Error
	if err != nil {
		return err
	}
	if err = DB.Delete(&group).Error; err != nil {
		return err
	}
	return nil
}

func GroupCount() (int64, error) {
	var count int64
	err := DB.Model(&Group{}).Count(&count).Error
	return count, err
}

// GetGroupModelRatio 返回指定组的倍率乘数。
// default 组始终返回 1.0；其他组返回 DB 中存储的 ModelRatio（最小 0.001）。
func GetGroupModelRatio(group string) float64 {
	if group == "" || group == "default" {
		return 1.0
	}
	groupObj, err := GetGroupByName(group)
	if err != nil || groupObj == nil {
		return 1.0
	}
	if groupObj.ModelRatio <= 0 {
		return 1.0
	}
	return groupObj.ModelRatio
}
