package model

import (
	"encoding/json"
	"errors"
	"strings"

	"gorm.io/gorm"

	"github.com/songquanpeng/one-api/common/logger"
)

const (
	TokenStatusEnabled  = 1
	TokenStatusDisabled = 2
)

type Token struct {
	Id             int     `json:"id"`
	UserId         int     `json:"user_id"`
	Key            string  `json:"key" gorm:"type:char(48);uniqueIndex"`
	Status         int     `json:"status" gorm:"default:1"`
	Name           string  `json:"name" gorm:"index" `
	CreatedTime  int64   `json:"created_time" gorm:"bigint"`
	AccessedTime int64   `json:"accessed_time" gorm:"bigint"`
	Models       *string `json:"models" gorm:"type:text"`               // allowed models
	Subnet       *string `json:"subnet" gorm:"default:''"`             // allowed subnet
	ModelMapping *string `json:"model_mapping" gorm:"type:varchar(1024);default:''"`
}

func GetAllUserTokens(userId int, startIdx int, num int) ([]*Token, error) {
	var tokens []*Token
	var err error
	query := DB.Where("user_id = ?", userId).Order("id desc")

	err = query.Limit(num).Offset(startIdx).Find(&tokens).Error
	return tokens, err
}

func SearchUserTokens(userId int, keyword string) (tokens []*Token, err error) {
	err = DB.Where("user_id = ?", userId).Where("name LIKE ?", keyword+"%").Find(&tokens).Error
	return tokens, err
}

func ValidateUserToken(key string) (token *Token, err error) {
	if key == "" {
		return nil, errors.New("未提供令牌")
	}
	token, err = CacheGetTokenByKey(key)
	if err != nil {
		logger.Log.Errorf("CacheGetTokenByKey failed: " + err.Error())
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("无效的令牌")
		}
		return nil, errors.New("令牌验证失败")
	}
	if token.Status != TokenStatusEnabled {
		return nil, errors.New("该令牌状态不可用")
	}
	return token, nil
}

func GetTokenByIds(id int, userId int) (*Token, error) {
	if id == 0 || userId == 0 {
		return nil, errors.New("id 或 userId 为空！")
	}
	token := Token{Id: id, UserId: userId}
	var err error = nil
	err = DB.First(&token, "id = ? and user_id = ?", id, userId).Error
	return &token, err
}

func GetTokenById(id int) (*Token, error) {
	if id == 0 {
		return nil, errors.New("id 为空！")
	}
	token := Token{Id: id}
	var err error = nil
	err = DB.First(&token, "id = ?", id).Error
	return &token, err
}

func (t *Token) Insert() error {
	var err error
	err = DB.Create(t).Error
	return err
}

// Update Make sure your token's fields is completed, because this will update non-zero values
func (t *Token) Update() error {
	var err error
	err = DB.Model(t).Select("name", "status", "models", "subnet", "model_mapping").Updates(t).Error
	return err
}

func (t *Token) Delete() error {
	var err error
	err = DB.Delete(t).Error
	return err
}

func SimplifyModelsField(models *string) *string {
	if models == nil {
		return nil
	}
	if *models == "" {
		return models
	}
	modelList := strings.Split(*models, ",")
	var aliases []string
	for _, m := range modelList {
		aliases = append(aliases, SimplifyModelName(m))
	}
	simplified := strings.Join(aliases, ",")
	return &simplified
}

func (t *Token) GetModels() string {
	if t == nil {
		return ""
	}
	if t.Models == nil {
		return ""
	}
	return *t.Models
}

func (t *Token) GetModelMapping() map[string]string {
	if t == nil || t.ModelMapping == nil || *t.ModelMapping == "" || *t.ModelMapping == "{}" {
		return nil
	}
	modelMapping := make(map[string]string)
	err := json.Unmarshal([]byte(*t.ModelMapping), &modelMapping)
	if err != nil {
		logger.Log.Errorf("failed to unmarshal model mapping for token %d, error: %s", t.Id, err.Error())
		return nil
	}
	return modelMapping
}

func DeleteTokenById(id int, userId int) (err error) {
	// Why we need userId here? In case user want to delete other's token.
	if id == 0 || userId == 0 {
		return errors.New("id 或 userId 为空！")
	}
	token := Token{Id: id, UserId: userId}
	err = DB.Where(token).First(&token).Error
	if err != nil {
		return err
	}
	return token.Delete()
}
