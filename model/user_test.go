package model

import (
	"testing"
	"time"

	"github.com/pai801/myapi/common"
	. "github.com/smartystreets/goconvey/convey"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// initTestUserDB sets up an in-memory SQLite DB with one enabled user.
// Redis is left disabled so cache functions fall through to DB queries.
func initTestUserDB(t *testing.T) *User {
	t.Helper()
	common.RedisEnabled = false
	common.UsingSQLite = true

	var err error
	DB, err = gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open in-memory db: %v", err)
	}
	_ = DB.AutoMigrate(&User{})

	user := &User{
		Username: "testuser",
		Status:   UserStatusEnabled,
	}
	if err := DB.Create(user).Error; err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	return user
}

func TestUserEnabledCacheInvalidationAfterStatusChange(t *testing.T) {
	Convey("Given one enabled user, status changes are visible to the cache layer immediately", t, func() {
		user := initTestUserDB(t)

		enabled, err := CacheIsUserEnabled(user.Id)
		So(err, ShouldBeNil)
		So(enabled, ShouldBeTrue)

		Convey("After disabling via Update, user is no longer enabled", func() {
			user.Status = UserStatusDisabled
			So(user.Update(false, "status"), ShouldBeNil)

			enabled, err := CacheIsUserEnabled(user.Id)
			So(err, ShouldBeNil)
			So(enabled, ShouldBeFalse)
		})

		Convey("After disable then enable via Update, user is enabled again", func() {
			user.Status = UserStatusDisabled
			So(user.Update(false, "status"), ShouldBeNil)

			user.Status = UserStatusEnabled
			So(user.Update(false, "status"), ShouldBeNil)

			enabled, err := CacheIsUserEnabled(user.Id)
			So(err, ShouldBeNil)
			So(enabled, ShouldBeTrue)
		})

		Convey("After Delete, user is no longer enabled", func() {
			So(user.Delete(), ShouldBeNil)

			enabled, err := CacheIsUserEnabled(user.Id)
			So(err, ShouldBeNil)
			So(enabled, ShouldBeFalse)
		})
	})
}

// UpdateUser/UpdateSelf 均经 User.Update 的列白名单路径：
// 验证白名单列零值可写入（quota 改 0、清空 display_name）、白名单外敏感列不被写
// （UpdateSelf 传参形态不能改 role/quota/status）、空 username 不会被清空
func TestUserUpdateColumnWhitelist(t *testing.T) {
	Convey("Given one user with non-zero quota/role/display_name", t, func() {
		user := initTestUserDB(t)
		err := DB.Model(&User{}).Where("id = ?", user.Id).Updates(map[string]interface{}{
			"quota":        5000,
			"role":         RoleCommonUser,
			"display_name": "old-name",
		}).Error
		So(err, ShouldBeNil)

		Convey("Zero values of whitelisted columns are written", func() {
			fresh := &User{Id: user.Id}
			So(fresh.Update(false, "username", "display_name", "role", "status", "quota"), ShouldBeNil)

			var got User
			So(DB.First(&got, "id = ?", user.Id).Error, ShouldBeNil)
			So(got.Quota, ShouldEqual, 0)
			So(got.DisplayName, ShouldEqual, "")
			So(got.Role, ShouldEqual, RoleGuestUser)
		})

		Convey("UpdateSelf column set cannot change role/quota/status", func() {
			// 与 UpdateSelf 控制器完全一致的传参形态：仅 username/display_name(+password)
			self := &User{Id: user.Id, Username: "testuser", DisplayName: "self"}
			So(self.Update(false, "username", "display_name"), ShouldBeNil)

			var got User
			So(DB.First(&got, "id = ?", user.Id).Error, ShouldBeNil)
			So(got.Role, ShouldEqual, RoleCommonUser)
			So(got.Status, ShouldEqual, UserStatusEnabled)
			So(got.Quota, ShouldEqual, 5000)
			So(got.DisplayName, ShouldEqual, "self")
		})

		Convey("Empty username is skipped and password untouched without updatePassword", func() {
			self := &User{Id: user.Id, Username: "", DisplayName: "x"}
			So(self.Update(false, "username", "display_name"), ShouldBeNil)

			var got User
			So(DB.First(&got, "id = ?", user.Id).Error, ShouldBeNil)
			So(got.Username, ShouldEqual, "testuser")
			So(got.Password, ShouldEqual, "")
		})
	})
}

// TestUserUpdateQuotaCacheRefreshScope 验证 Update 的额度缓存刷新仅跟随 quota 列变更：
// 非 quota 更新（UpdateSelf/GenerateAccessToken 等路径）不得回写额度缓存，
// quota 更新仍必须回源，否则 TTL 窗口内额度检查使用旧值
func TestUserUpdateQuotaCacheRefreshScope(t *testing.T) {
	Convey("Redis 开启时，额度缓存回写仅跟随 quota 列变更", t, func() {
		setupRedisTest(t)
		user := initRedisTestUserDB(t, 5000)

		// 预置与 DB 值不同的缓存哨兵值：非 quota 更新若误刷新缓存会被 DB 值覆盖而暴露
		So(common.RedisSet(userQuotaCacheKey(user.Id), "777777", 60*time.Second), ShouldBeNil)

		Convey("非 quota 更新不触发额度缓存刷新", func() {
			user.DisplayName = "renamed"
			So(user.Update(false, "display_name"), ShouldBeNil)

			val, err := common.RedisGet(userQuotaCacheKey(user.Id))
			So(err, ShouldBeNil)
			So(val, ShouldEqual, "777777")
		})

		Convey("quota 更新仍触发额度缓存刷新（回源 DB 绝对值）", func() {
			user.Quota = 8888
			So(user.Update(false, "quota"), ShouldBeNil)

			val, err := common.RedisGet(userQuotaCacheKey(user.Id))
			So(err, ShouldBeNil)
			So(val, ShouldEqual, "8888")
		})
	})
}
