package model

import (
	"fmt"
	"sync"
	"testing"

	"github.com/pai801/myapi/common"
	. "github.com/smartystreets/goconvey/convey"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestChannelModelsAlias(t *testing.T) {
	Convey("Channel struct should have ModelsAlias field", t, func() {
		c := Channel{ModelsAlias: "test-model"}
		So(c.ModelsAlias, ShouldEqual, "test-model")
	})
}

func TestSimplifyModelName(t *testing.T) {
	Convey("SimplifyModelName should remove non-alphanumeric chars and lowercase", t, func() {
		So(SimplifyModelName("gpt-4-turbo"), ShouldEqual, "gpt4turbo")
	})
}

func TestAutoGenerateModelsAlias(t *testing.T) {
	Convey("autoGenerateModelsAlias generates alias from Models", t, func() {
		c := Channel{Models: "gpt-4-turbo,gpt-3.5-turbo"}
		c.autoGenerateModelsAlias()
		So(c.ModelsAlias, ShouldEqual, "gpt4turbo,gpt35turbo")
	})

	Convey("autoGenerateModelsAlias sets empty when Models is empty", t, func() {
		c := Channel{Models: ""}
		c.autoGenerateModelsAlias()
		So(c.ModelsAlias, ShouldEqual, "")
	})
}

// 并发 hammer 锁住 GetModels/GetAlias 懒初始化的加锁实现：
// go test -race 下并发调用（含首次初始化）不得出现 DATA RACE
func TestGetModelsAliasConcurrent(t *testing.T) {
	Convey("16 goroutines concurrently call GetModels/GetAlias on one instance", t, func() {
		c := &Channel{Id: 987654, Models: "m1,m2,m3", ModelsAlias: "a1,a2,a3"}

		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 100; j++ {
					_ = c.GetModels()
					_ = c.GetAlias()
				}
			}()
		}
		wg.Wait()

		So(c.GetModels(), ShouldResemble, []string{"m1", "m2", "m3"})
		So(c.GetAlias(), ShouldResemble, []string{"a1", "a2", "a3"})
	})
}

// TestBatchDeleteChannelsChunked 覆盖 M5 分片删除：渠道数超过单批 500 时按片清理，
// 验证分片循环完整执行、affected 为各片汇总、abilities 一并清理
func TestBatchDeleteChannelsChunked(t *testing.T) {
	Convey("Given 1200 disabled channels, batch delete removes all in 500-sized chunks", t, func() {
		common.UsingSQLite = true
		var err error
		DB, err = gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
		if err != nil {
			t.Fatalf("failed to open in-memory db: %v", err)
		}
		_ = DB.AutoMigrate(&Channel{}, &Ability{})

		channels := make([]Channel, 0, 1200)
		for i := 0; i < 1200; i++ {
			channels = append(channels, Channel{
				Name:   fmt.Sprintf("ch-%d", i),
				Status: ChannelStatusAutoDisabled,
				Group:  "default",
				Models: "m1",
				Type:   1,
				Key:    "sk-test",
			})
		}
		// 分批插入，避免单条 INSERT 宿主参数数超限
		So(DB.CreateInBatches(&channels, 100).Error, ShouldBeNil)
		for i := 0; i < 3; i++ {
			So(DB.Create(&Ability{Group: "default", Model: "m1", ChannelId: channels[i].Id, Enabled: true}).Error, ShouldBeNil)
		}

		// 删除前重新启用的渠道不进入候选集合，其 abilities 一并保留
		reEnabled := []int{channels[0].Id, channels[1].Id, channels[2].Id}
		So(DB.Model(&Channel{}).Where("id IN ?", reEnabled).Update("status", ChannelStatusEnabled).Error, ShouldBeNil)

		affected, err := DeleteDisabledChannel()
		So(err, ShouldBeNil)
		So(affected, ShouldEqual, 1197)

		var count int64
		So(DB.Model(&Channel{}).Where("status = ?", ChannelStatusEnabled).Count(&count).Error, ShouldBeNil)
		So(count, ShouldEqual, 3)
		So(DB.Model(&Ability{}).Count(&count).Error, ShouldBeNil)
		So(count, ShouldEqual, 3)

		// DeleteChannelByStatus 走同一分片路径：另取 2 个启用渠道改手动禁用后按状态清理
		So(DB.Model(&Channel{}).Where("id IN ?", reEnabled[:2]).Update("status", ChannelStatusManuallyDisabled).Error, ShouldBeNil)

		affected, err = DeleteChannelByStatus(ChannelStatusManuallyDisabled)
		So(err, ShouldBeNil)
		So(affected, ShouldEqual, 2)

		So(DB.Model(&Channel{}).Count(&count).Error, ShouldBeNil)
		So(count, ShouldEqual, 1)
		So(DB.Model(&Ability{}).Count(&count).Error, ShouldBeNil)
		So(count, ShouldEqual, 1)
	})
}