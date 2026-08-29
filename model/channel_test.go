package model

import (
	"sync"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
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