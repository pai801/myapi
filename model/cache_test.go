package model

import (
	"testing"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
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