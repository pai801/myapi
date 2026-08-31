package model

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/pai801/myapi/common"
	. "github.com/smartystreets/goconvey/convey"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupRedisTest 启动进程内 miniredis 并接管全局 Redis 开关与客户端。
// 全局状态（common.RedisEnabled/common.RDB）在测试结束恢复原值，避免污染同包其他用例。
func setupRedisTest(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	mr := miniredis.RunT(t)
	prevEnabled, prevRDB := common.RedisEnabled, common.RDB
	common.RedisEnabled = true
	common.RDB = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		if client, ok := common.RDB.(*redis.Client); ok {
			_ = client.Close()
		}
		common.RedisEnabled = prevEnabled
		common.RDB = prevRDB
	})
	return mr
}

// initRedisTestTokenDB 建内存 SQLite 并写入一个启用令牌（与 initTestTokenDB 的差异：不关 Redis）
func initRedisTestTokenDB(t *testing.T) *Token {
	t.Helper()
	common.UsingSQLite = true

	var err error
	DB, err = gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open in-memory db: %v", err)
	}
	_ = DB.AutoMigrate(&Token{})

	token := &Token{
		UserId: 1,
		Key:    "redis-cache-test-token-key-000000000000000",
		Status: TokenStatusEnabled,
	}
	if err := DB.Create(token).Error; err != nil {
		t.Fatalf("failed to create token: %v", err)
	}
	return token
}

// initRedisTestUserDB 建内存 SQLite 并写入一个启用、带额度的用户
func initRedisTestUserDB(t *testing.T, quota int64) *User {
	t.Helper()
	common.UsingSQLite = true

	var err error
	DB, err = gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open in-memory db: %v", err)
	}
	_ = DB.AutoMigrate(&User{})

	user := &User{
		Username: "redis-user",
		Status:   UserStatusEnabled,
		Quota:    quota,
	}
	if err := DB.Create(user).Error; err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	return user
}

func TestTokenCacheInvalidationWithRedisEnabled(t *testing.T) {
	Convey("Redis 开启时令牌写路径失效缓存，读侧立即看到新值", t, func() {
		mr := setupRedisTest(t)
		token := initRedisTestTokenDB(t)

		// 首读回源并填充缓存
		tok, err := CacheGetTokenByKey(token.Key)
		So(err, ShouldBeNil)
		So(tok.Status, ShouldEqual, TokenStatusEnabled)
		So(mr.Exists(tokenCacheKey(token.Key)), ShouldBeTrue)

		Convey("写路径（Update 内部失效）后缓存读到新状态", func() {
			token.Status = TokenStatusDisabled
			So(token.Update(), ShouldBeNil)

			tok, err := CacheGetTokenByKey(token.Key)
			So(err, ShouldBeNil)
			So(tok.Status, ShouldEqual, TokenStatusDisabled)
		})

		Convey("未失效时读到旧缓存值，手动失效后回源读到新值", func() {
			// 绕过写路径直改 DB，缓存保持旧值，证明读的确实是缓存而非 DB
			if err := DB.Model(&Token{}).Where("id = ?", token.Id).Update("status", TokenStatusDisabled).Error; err != nil {
				t.Fatalf("failed to update token status: %v", err)
			}
			tok, err := CacheGetTokenByKey(token.Key)
			So(err, ShouldBeNil)
			So(tok.Status, ShouldEqual, TokenStatusEnabled)

			So(CacheInvalidateTokenByKey(token.Key), ShouldBeNil)
			So(mr.Exists(tokenCacheKey(token.Key)), ShouldBeFalse)

			tok, err = CacheGetTokenByKey(token.Key)
			So(err, ShouldBeNil)
			So(tok.Status, ShouldEqual, TokenStatusDisabled)
		})

		Convey("Delete 后缓存失效，校验立即拒绝", func() {
			So(token.Delete(), ShouldBeNil)
			So(mr.Exists(tokenCacheKey(token.Key)), ShouldBeFalse)

			_, verr := ValidateUserToken(token.Key)
			So(verr, ShouldNotBeNil)
			So(verr.Error(), ShouldEqual, "无效的令牌")
		})
	})
}

func TestUserEnabledCacheInvalidationWithRedisEnabled(t *testing.T) {
	Convey("Redis 开启时用户状态写路径失效缓存，读侧立即看到新状态", t, func() {
		setupRedisTest(t)
		user := initRedisTestUserDB(t, 0)

		enabled, err := CacheIsUserEnabled(user.Id)
		So(err, ShouldBeNil)
		So(enabled, ShouldBeTrue)

		Convey("Update 禁用（内部失效缓存）后立即读到禁用", func() {
			user.Status = UserStatusDisabled
			So(user.Update(false, "status"), ShouldBeNil)

			enabled, err := CacheIsUserEnabled(user.Id)
			So(err, ShouldBeNil)
			So(enabled, ShouldBeFalse)
		})

		Convey("手动失效后回源读到新状态", func() {
			// 绕过写路径直改 DB，缓存保持旧值，证明读的确实是缓存
			So(DB.Model(&User{}).Where("id = ?", user.Id).Update("status", UserStatusDisabled).Error, ShouldBeNil)
			enabled, err := CacheIsUserEnabled(user.Id)
			So(err, ShouldBeNil)
			So(enabled, ShouldBeTrue)

			So(CacheInvalidateUserEnabled(user.Id), ShouldBeNil)
			enabled, err = CacheIsUserEnabled(user.Id)
			So(err, ShouldBeNil)
			So(enabled, ShouldBeFalse)
		})
	})
}

// TestUserQuotaDeltaScriptWithRedis 覆盖额度增量 Lua 脚本的两分支：
// 键不存在跳过（不凭空建键）、键存在原子增量并续期 TTL
func TestUserQuotaDeltaScriptWithRedis(t *testing.T) {
	Convey("Redis 开启时额度增量 Lua 脚本按键是否存在分流", t, func() {
		mr := setupRedisTest(t)
		user := initRedisTestUserDB(t, 1000)

		// 用较长 TTL 使"续期生效"可观测（初值 60s → 续期后 300s）
		prevTTL := UserId2QuotaCacheSeconds
		UserId2QuotaCacheSeconds = 300
		t.Cleanup(func() { UserId2QuotaCacheSeconds = prevTTL })

		Convey("键不存在时跳过：不凭空建键，回源返回 DB 真实值", func() {
			So(CacheDecreaseUserQuota(user.Id, 100), ShouldBeNil)
			So(mr.Exists(userQuotaCacheKey(user.Id)), ShouldBeFalse)

			quota, err := CacheGetUserQuota(context.Background(), user.Id)
			So(err, ShouldBeNil)
			So(quota, ShouldEqual, 1000)
		})

		Convey("键存在时增量落值并续期 TTL", func() {
			So(common.RedisSet(userQuotaCacheKey(user.Id), "1000", 60*time.Second), ShouldBeNil)
			// 键存在且带 60s TTL（TTL 对不存在的键返回 0）
			So(mr.Exists(userQuotaCacheKey(user.Id)), ShouldBeTrue)
			So(mr.TTL(userQuotaCacheKey(user.Id)), ShouldBeLessThanOrEqualTo, 60*time.Second)

			So(CacheDecreaseUserQuota(user.Id, 100), ShouldBeNil)
			val, err := mr.Get(userQuotaCacheKey(user.Id))
			So(err, ShouldBeNil)
			So(val, ShouldEqual, "900")

			So(CacheIncreaseUserQuota(user.Id, 500), ShouldBeNil)
			val, err = mr.Get(userQuotaCacheKey(user.Id))
			So(err, ShouldBeNil)
			So(val, ShouldEqual, "1400")

			// 续期：TTL 被刷新到 UserId2QuotaCacheSeconds（300s），远大于初始 60s
			So(mr.TTL(userQuotaCacheKey(user.Id)), ShouldBeGreaterThan, 60*time.Second)
		})
	})
}
