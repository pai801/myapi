package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"gorm.io/gorm"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/blacklist"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/helper"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/common/random"
)

const (
	RoleGuestUser  = 0
	RoleCommonUser = 1
	RoleAdminUser  = 10
	RoleRootUser   = 100
)

const (
	UserStatusEnabled  = 1 // don't use 0, 0 is the default value!
	UserStatusDisabled = 2 // also don't use 0
	UserStatusDeleted  = 3
)

// User if you add sensitive fields, don't forget to clean them in setupLogin function.
// Otherwise, the sensitive information will be saved on local storage in plain text!
type User struct {
	Id           int    `json:"id"`
	Username     string `json:"username" gorm:"unique;index" validate:"max=12"`
	Password     string `json:"password" gorm:"not null;" validate:"min=8,max=20"`
	DisplayName  string `json:"display_name" gorm:"index" validate:"max=20"`
	Role         int    `json:"role" gorm:"type:int;default:1"`   // admin, util
	Status       int    `json:"status" gorm:"type:int;default:1"` // enabled, disabled
	Email        string `json:"email" gorm:"index" validate:"max=50"`
	AccessToken  string `json:"access_token" gorm:"type:char(32);column:access_token;uniqueIndex"` // this token is for system management
	Quota        int64  `json:"quota" gorm:"bigint;default:0"`
	UsedQuota    int64  `json:"used_quota" gorm:"bigint;default:0;column:used_quota"` // used quota
	RequestCount int    `json:"request_count" gorm:"type:int;default:0;"`             // request number
}

func GetMaxUserId() int {
	var user User
	DB.Last(&user)
	return user.Id
}

func GetAllUsers(startIdx int, num int) (users []*User, err error) {
	err = DB.Omit("password", "access_token").Where("status != ?", UserStatusDeleted).Order("id desc").Limit(num).Offset(startIdx).Find(&users).Error
	return users, err
}

func SearchUsers(keyword string) (users []*User, err error) {
	if !common.UsingPostgreSQL {
		err = DB.Omit("password", "access_token").Where("id = ? or username LIKE ? or email LIKE ? or display_name LIKE ?", keyword, keyword+"%", keyword+"%", keyword+"%").Find(&users).Error
	} else {
		err = DB.Omit("password", "access_token").Where("username LIKE ? or email LIKE ? or display_name LIKE ?", keyword+"%", keyword+"%", keyword+"%").Find(&users).Error
	}
	return users, err
}

func GetUserById(id int, selectAll bool) (*User, error) {
	if id == 0 {
		return nil, errors.New("id 为空！")
	}
	user := User{Id: id}
	var err error = nil
	if selectAll {
		err = DB.First(&user, "id = ?", id).Error
	} else {
		err = DB.Omit("password", "access_token").First(&user, "id = ?", id).Error
	}
	return &user, err
}

func DeleteUserById(id int) (err error) {
	if id == 0 {
		return errors.New("id 为空！")
	}
	user := User{Id: id}
	return user.Delete()
}

func (user *User) Insert(ctx context.Context) error {
	var err error
	if user.Password != "" {
		user.Password, err = common.Password2Hash(user.Password)
		if err != nil {
			return err
		}
	}
	user.Quota = 0
	user.AccessToken = random.GetUUID()
	result := DB.Create(user)
	if result.Error != nil {
		return result.Error
	}
	// create default token
	cleanToken := Token{
		UserId:       user.Id,
		Name:         "default",
		Key:          random.GenerateKey(),
		CreatedTime:  helper.GetTimestamp(),
		AccessedTime: helper.GetTimestamp(),
	}
	result.Error = cleanToken.Insert()
	if result.Error != nil {
		// do not block
		logger.Log.Errorf("create default token for user %d failed: %s", user.Id, result.Error.Error())
	}
	return nil
}

// Update 按显式列白名单写入。GORM struct Updates 会静默忽略零值字段，导致管理员把
// quota 改为 0、清空 display_name 等操作失效，故改为 Select 白名单：选中列的零值也会落库。
// password 不由调用方传入白名单，仅当 updatePassword 为 true 时自动写入（避免空串清空密码）；
// username 为空串时跳过写入，避免把登录名清空（保持原先零值被忽略时的保护语义）。
// 各调用方白名单：UpdateUser(username/display_name/role/status，quota 仅在请求显式携带时附加，
// 见 controller 的 updateUserRequest)、UpdateSelf(username/display_name)、ManageUser(status)、
// GenerateAccessToken(access_token)——UpdateSelf 等非管理员路径的敏感列
// （role/quota/status/email 等）不进白名单，权限保护不再依赖"零值被忽略"的副作用。
func (user *User) Update(updatePassword bool, columns ...string) error {
	var err error
	if updatePassword {
		user.Password, err = common.Password2Hash(user.Password)
		if err != nil {
			return err
		}
	}
	selected := make([]string, 0, len(columns)+1)
	for _, col := range columns {
		if col == "username" && user.Username == "" {
			continue
		}
		selected = append(selected, col)
	}
	if updatePassword {
		selected = append(selected, "password")
	}
	if len(selected) == 0 {
		return errors.New("没有可更新的字段")
	}
	if user.Status == UserStatusDisabled {
		blacklist.BanUser(user.Id)
	} else if user.Status == UserStatusEnabled {
		blacklist.UnbanUser(user.Id)
	}
	err = DB.Model(user).Select(selected).Updates(user).Error
	if err == nil {
		// ManageUser 禁用/启用、UpdateUser 等均经此路径，需同步失效用户状态缓存
		_ = CacheInvalidateUserEnabled(user.Id)
		// 管理员修正额度经此路径直写 DB 绝对值（不经 Increase/DecreaseUserQuota 增量路径），
		// 必须回源刷新额度缓存，否则 TTL 窗口内额度检查仍用旧值；
		// 仅本次更新列含 quota 时刷新，非额度更新（UpdateSelf/GenerateAccessToken 等）不产生该开销
		if common.RedisEnabled && slices.Contains(selected, "quota") {
			if uerr := CacheUpdateUserQuota(context.Background(), user.Id); uerr != nil {
				logger.Log.Errorf("update user quota cache for user %d failed: %s", user.Id, uerr.Error())
			}
		}
	}
	return err
}

// Deprecated: user deletion is no longer supported after removing operational features.
func (user *User) Delete() error {
	if user.Id == 0 {
		return errors.New("id 为空！")
	}
	blacklist.BanUser(user.Id)
	user.Username = fmt.Sprintf("deleted_%s", random.GetUUID())
	user.Status = UserStatusDeleted
	err := DB.Model(user).Updates(user).Error
	if err == nil {
		_ = CacheInvalidateUserEnabled(user.Id)
	}
	return err
}

// ValidateAndFill check password & user status
func (user *User) ValidateAndFill() (err error) {
	// When querying with struct, GORM will only query with non-zero fields,
	// that means if your field’s value is 0, '', false or other zero values,
	// it won’t be used to build query conditions
	password := user.Password
	if user.Username == "" || password == "" {
		return errors.New("用户名或密码为空")
	}
	err = DB.Where("username = ?", user.Username).First(user).Error
	if err != nil {
		// we must make sure check username firstly
		// consider this case: a malicious user set his username as other's email
		err := DB.Where("email = ?", user.Username).First(user).Error
		if err != nil {
			return errors.New("用户名或密码错误，或用户已被封禁")
		}
	}
	okay := common.ValidatePasswordAndHash(password, user.Password)
	if !okay || user.Status != UserStatusEnabled {
		return errors.New("用户名或密码错误，或用户已被封禁")
	}
	return nil
}

func (user *User) FillUserById() error {
	if user.Id == 0 {
		return errors.New("id 为空！")
	}
	DB.Where(User{Id: user.Id}).First(user)
	return nil
}

func (user *User) FillUserByEmail() error {
	if user.Email == "" {
		return errors.New("email 为空！")
	}
	DB.Where(User{Email: user.Email}).First(user)
	return nil
}

func (user *User) FillUserByUsername() error {
	if user.Username == "" {
		return errors.New("username 为空！")
	}
	DB.Where(User{Username: user.Username}).First(user)
	return nil
}

func IsEmailAlreadyTaken(email string) bool {
	return DB.Where("email = ?", email).Find(&User{}).RowsAffected == 1
}

func IsUsernameAlreadyTaken(username string) bool {
	return DB.Where("username = ?", username).Find(&User{}).RowsAffected == 1
}

func ResetUserPasswordByEmail(email string, password string) error {
	if email == "" || password == "" {
		return errors.New("邮箱地址或密码为空！")
	}
	hashedPassword, err := common.Password2Hash(password)
	if err != nil {
		return err
	}
	err = DB.Model(&User{}).Where("email = ?", email).Update("password", hashedPassword).Error
	return err
}

func IsAdmin(userId int) bool {
	if userId == 0 {
		return false
	}
	var user User
	err := DB.Where("id = ?", userId).Select("role").Find(&user).Error
	if err != nil {
		logger.Log.Errorf("no such user " + err.Error())
		return false
	}
	return user.Role >= RoleAdminUser
}

func IsUserEnabled(userId int) (bool, error) {
	if userId == 0 {
		return false, errors.New("user id is empty")
	}
	var user User
	err := DB.Where("id = ?", userId).Select("status").Find(&user).Error
	if err != nil {
		return false, err
	}
	return user.Status == UserStatusEnabled, nil
}

func ValidateAccessToken(token string) (user *User) {
	if token == "" {
		return nil
	}
	token = strings.Replace(token, "Bearer ", "", 1)
	user = &User{}
	if DB.Where("access_token = ?", token).First(user).RowsAffected == 1 {
		return user
	}
	return nil
}

func GetUserQuota(id int) (quota int64, err error) {
	err = DB.Model(&User{}).Where("id = ?", id).Select("quota").Find(&quota).Error
	return quota, err
}

func GetUserUsedQuota(id int) (quota int64, err error) {
	err = DB.Model(&User{}).Where("id = ?", id).Select("used_quota").Find(&quota).Error
	return quota, err
}

func GetUserEmail(id int) (email string, err error) {
	err = DB.Model(&User{}).Where("id = ?", id).Select("email").Find(&email).Error
	return email, err
}

func IncreaseUserQuota(id int, quota int64) (err error) {
	if quota < 0 {
		return errors.New("quota 不能为负数！")
	}
	if config.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeUserQuota, id, quota)
	} else {
		if err = increaseUserQuota(id, quota); err != nil {
			return err
		}
	}
	// 退回/回滚同样增量同步 Redis 缓存，维持"缓存值 = DB 已落盘值 + 批量缓冲"恒等式；
	// 同步失败仅记日志：DB 为权威状态，缓存误差由 PostConsumeReset/回源/TTL 收敛
	if common.RedisEnabled {
		if cerr := CacheIncreaseUserQuota(id, quota); cerr != nil {
			logger.Log.Errorf("increase user quota cache for user %d failed: %s", id, cerr.Error())
		}
	}
	return nil
}

func increaseUserQuota(id int, quota int64) (err error) {
	err = DB.Model(&User{}).Where("id = ?", id).Update("quota", gorm.Expr("quota + ?", quota)).Error
	return err
}

func DecreaseUserQuota(id int, quota int64) (err error) {
	if quota < 0 {
		return errors.New("quota 不能为负数！")
	}
	if config.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeUserQuota, id, -quota)
	} else {
		if err = decreaseUserQuota(id, quota); err != nil {
			return err
		}
	}
	// 预扣/扣费成功后增量同步扣减 Redis 缓存：额度检查读的是缓存，
	// 若不同步，批量间隔或 TTL 窗口内预扣对后续请求不可见，可被高并发持续透支
	// （同步失败仅记日志：DB 为权威状态，缓存误差由 PostConsumeReset/回源/TTL 收敛）
	if common.RedisEnabled {
		if cerr := CacheDecreaseUserQuota(id, quota); cerr != nil {
			logger.Log.Errorf("decrease user quota cache for user %d failed: %s", id, cerr.Error())
		}
	}
	return nil
}

func decreaseUserQuota(id int, quota int64) (err error) {
	err = DB.Model(&User{}).Where("id = ?", id).Update("quota", gorm.Expr("quota - ?", quota)).Error
	return err
}

// PostConsumeResetUserQuotaCache 在 DB 扣费完成后刷新用户额度缓存。
// 批量模式下 DB 值尚未落盘，CacheUpdateUserQuota 会叠加批量缓冲差值后回写，
// 避免用陈旧 DB 值覆盖缓存（见 getPendingUserQuotaDelta）；
// 非批量模式 DB 已落盘，直接回读回写。
func PostConsumeResetUserQuotaCache(ctx context.Context, userId int, consumedQuota int64) {
	if common.RedisEnabled {
		if err := CacheUpdateUserQuota(ctx, userId); err != nil {
			logger.Log.Errorf("error update user quota cache: " + err.Error())
		}
	}
}

func GetRootUserEmail() (email string) {
	DB.Model(&User{}).Where("role = ?", RoleRootUser).Select("email").Find(&email)
	return email
}

func UpdateUserUsedQuotaAndRequestCount(id int, quota int64) {
	if config.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeUsedQuota, id, quota)
		addNewRecord(BatchUpdateTypeRequestCount, id, 1)
		return
	}
	updateUserUsedQuotaAndRequestCount(id, quota, 1)
}

func updateUserUsedQuotaAndRequestCount(id int, quota int64, count int) {
	err := DB.Model(&User{}).Where("id = ?", id).Updates(
		map[string]interface{}{
			"used_quota":    gorm.Expr("used_quota + ?", quota),
			"request_count": gorm.Expr("request_count + ?", count),
		},
	).Error
	if err != nil {
		logger.Log.Errorf("failed to update user used quota and request count: " + err.Error())
	}
}

func updateUserUsedQuota(id int, quota int64) {
	err := DB.Model(&User{}).Where("id = ?", id).Updates(
		map[string]interface{}{
			"used_quota": gorm.Expr("used_quota + ?", quota),
		},
	).Error
	if err != nil {
		logger.Log.Errorf("failed to update user used quota: " + err.Error())
	}
}

func updateUserRequestCount(id int, count int) {
	err := DB.Model(&User{}).Where("id = ?", id).Update("request_count", gorm.Expr("request_count + ?", count)).Error
	if err != nil {
		logger.Log.Errorf("failed to update user request count: " + err.Error())
	}
}

func GetUsernameById(id int) (username string) {
	DB.Model(&User{}).Where("id = ?", id).Select("username").Find(&username)
	return username
}
