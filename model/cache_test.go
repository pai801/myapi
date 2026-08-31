package model

import (
	"context"
	"testing"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/config"
	. "github.com/smartystreets/goconvey/convey"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// initTestCacheDB sets up an in-memory SQLite DB and populates it with one
// enabled channel plus its ability entry. Also initializes the in-memory cache.
func initTestCacheDB(t *testing.T) {
	t.Helper()
	config.MemoryCacheEnabled = true
	common.UsingSQLite = true

	var err error
	DB, err = gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open in-memory db: %v", err)
	}
	_ = DB.AutoMigrate(&Channel{}, &Ability{})

	ch := &Channel{
		Name:   "test-channel",
		Status: ChannelStatusEnabled,
		Group:  "default",
		Models: "gpt-4",
		Type:   1,
		Key:    "sk-test",
	}
	if err := DB.Create(ch).Error; err != nil {
		t.Fatalf("failed to create channel: %v", err)
	}

	ab := &Ability{
		Group:     "default",
		Model:     "gpt-4",
		ChannelId: ch.Id,
		Enabled:   true,
	}
	if err := DB.Create(ab).Error; err != nil {
		t.Fatalf("failed to create ability: %v", err)
	}

	InitChannelCache()
}

func TestCacheInvalidationAfterChannelStatusChange(t *testing.T) {
	Convey("Given memory cache enabled with one enabled channel", t, func() {
		initTestCacheDB(t)

		Convey("CacheGetGroupChannels returns the enabled channel", func() {
			channels := CacheGetGroupChannels("default")
			So(len(channels), ShouldEqual, 1)
			So(channels[0].Id, ShouldEqual, 1)
		})

		Convey("After disabling, cache no longer returns the channel", func() {
			UpdateChannelStatusById(1, ChannelStatusManuallyDisabled)

			channels := CacheGetGroupChannels("default")
			So(len(channels), ShouldEqual, 0)
		})

		Convey("After disabling then re-enabling, cache returns it again", func() {
			UpdateChannelStatusById(1, ChannelStatusManuallyDisabled)
			UpdateChannelStatusById(1, ChannelStatusEnabled)

			channels := CacheGetGroupChannels("default")
			So(len(channels), ShouldEqual, 1)
			So(channels[0].Id, ShouldEqual, 1)
		})

		Convey("After Channel.Update with new models, cache reflects the change", func() {
			ch := &Channel{}
			DB.First(ch, 1)
			ch.Models = "gpt-4,gpt-5"
			err := ch.Update()
			So(err, ShouldBeNil)

			Convey("old model still cached", func() {
				ch2, err := CacheGetRandomSatisfiedChannel("default", "gpt-4", true)
				So(err, ShouldBeNil)
				So(ch2.Id, ShouldEqual, 1)
			})

			Convey("new model is in cache immediately", func() {
				ch2, err := CacheGetRandomSatisfiedChannel("default", "gpt-5", true)
				So(err, ShouldBeNil)
				So(ch2.Id, ShouldEqual, 1)
			})
		})

		Convey("After Channel.Update with new group, cache reflects the change", func() {
			ch := &Channel{}
			DB.First(ch, 1)
			ch.Group = "new-group"
			err := ch.Update()
			So(err, ShouldBeNil)

			Convey("channel is findable under new group", func() {
				ch2, err := CacheGetRandomSatisfiedChannel("new-group", "gpt-4", true)
				So(err, ShouldBeNil)
				So(ch2.Id, ShouldEqual, 1)
			})

			Convey("channel is no longer under old group in cache", func() {
				_, err := CacheGetRandomSatisfiedChannel("default", "gpt-4", true)
				So(err, ShouldNotBeNil)
			})
		})
	})
}
// initTestUserQuotaDB sets up an in-memory SQLite DB with one user for quota
// cache tests. Redis is disabled so cache functions fall through to DB queries.
func initTestUserQuotaDB(t *testing.T, quota int64) *User {
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
		Username: "quota-user",
		Status:   UserStatusEnabled,
		Quota:    quota,
	}
	if err := DB.Create(user).Error; err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	return user
}

func TestCacheGetUserQuotaFallsThroughToDB(t *testing.T) {
	Convey("Given redis disabled and a user with quota", t, func() {
		user := initTestUserQuotaDB(t, 12345)

		Convey("CacheGetUserQuota returns the DB value instead of 0", func() {
			quota, err := CacheGetUserQuota(context.Background(), user.Id)
			So(err, ShouldBeNil)
			So(quota, ShouldEqual, 12345)
		})
	})
}

func TestGetPendingUserQuotaDelta(t *testing.T) {
	Convey("Given records accumulated in the batch buffer", t, func() {
		// 使用隔离 id 避免与其它用例的真实用户 id 冲突
		const isolatedId = 987654
		addNewRecord(BatchUpdateTypeUserQuota, isolatedId, -200)
		addNewRecord(BatchUpdateTypeUserQuota, isolatedId, -50)
		Reset(func() {
			batchUpdateLocks[BatchUpdateTypeUserQuota].Lock()
			delete(batchUpdateStores[BatchUpdateTypeUserQuota], isolatedId)
			batchUpdateLocks[BatchUpdateTypeUserQuota].Unlock()
		})

		Convey("delta accumulates within the buffer", func() {
			So(getPendingUserQuotaDelta(isolatedId), ShouldEqual, -250)
		})

		Convey("unknown id returns zero", func() {
			So(getPendingUserQuotaDelta(isolatedId+1), ShouldEqual, 0)
		})
	})
}

func TestFetchAndUpdateUserQuotaIncludesPendingBatchDelta(t *testing.T) {
	Convey("Given batch mode with an unsettled pre-consumption in the buffer", t, func() {
		user := initTestUserQuotaDB(t, 1000)
		config.BatchUpdateEnabled = true
		Reset(func() {
			config.BatchUpdateEnabled = false
		})

		// 模拟预扣 200 已进批量缓冲但尚未落盘
		addNewRecord(BatchUpdateTypeUserQuota, user.Id, -200)
		Reset(func() {
			// 清理全局批量缓冲，避免残留差值污染同包后续用例
			batchUpdateLocks[BatchUpdateTypeUserQuota].Lock()
			delete(batchUpdateStores[BatchUpdateTypeUserQuota], user.Id)
			batchUpdateLocks[BatchUpdateTypeUserQuota].Unlock()
		})

		Convey("refetched quota equals DB value plus pending delta", func() {
			quota, err := fetchAndUpdateUserQuota(context.Background(), user.Id)
			So(err, ShouldBeNil)
			So(quota, ShouldEqual, 800)
		})
	})
}
