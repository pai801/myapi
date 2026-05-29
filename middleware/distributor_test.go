package middleware

import (
	"testing"

	"github.com/gin-gonic/gin"
	. "github.com/smartystreets/goconvey/convey"
	"github.com/songquanpeng/one-api/common/ctxkey"
	"github.com/songquanpeng/one-api/model"
)

func TestMatchChannelsByAliasExact(t *testing.T) {
	Convey("matchChannelsByAlias exact match", t, func() {
		channels := []*model.Channel{
			{Name: "A", Id: 1, ModelsAlias: "gpt4turbo,gpt35turbo"},
		}

		matched, _ := matchChannelsByAlias("gpt4turbo", channels)
		So(len(matched), ShouldEqual, 1)
		So(matched[0].Name, ShouldEqual, "A")
	})
}

func TestMatchChannelsByAliasPrefix(t *testing.T) {
	Convey("matchChannelsByAlias prefix match", t, func() {
		channels := []*model.Channel{
			{Name: "A", Id: 1, ModelsAlias: "gpt4turbo"},
			{Name: "B", Id: 2, ModelsAlias: "gpt41106preview"},
		}

		matched, _ := matchChannelsByAlias("gpt-4", channels)
		So(len(matched), ShouldEqual, 2)
	})
}

func TestSetDistributeContext(t *testing.T) {
	Convey("setDistributeContext sets all ctxkey values", t, func() {
		c, _ := gin.CreateTestContext(nil)
		channel := &model.Channel{Id: 42, Name: "test-ch", Type: 1}

		err := setDistributeContext(c, channel, "gpt-4-turbo", "gpt4turbo")
		So(err, ShouldBeNil)
		So(c.GetString(ctxkey.RequestModel), ShouldEqual, "gpt-4-turbo")
		So(c.GetString(ctxkey.SuggestedModel), ShouldEqual, "gpt4turbo")
		So(c.GetInt(ctxkey.ChannelId), ShouldEqual, 42)
		So(c.GetString(ctxkey.Group), ShouldEqual, "")
	})
}

func TestSelectAutoModel(t *testing.T) {
	Convey("selectAutoModel picks first model from ModelsAlias when available", t, func() {
		ch := &model.Channel{Models: "gpt-4,gpt-3.5-turbo", ModelsAlias: "gpt4,gpt35"}
		So(selectAutoModel(ch), ShouldEqual, "gpt4")
	})

	Convey("selectAutoModel falls back to Models when no alias", t, func() {
		ch := &model.Channel{Models: "gpt-4,gpt-3.5-turbo"}
		So(selectAutoModel(ch), ShouldEqual, "gpt-4")
	})

	Convey("selectAutoModel returns empty string when no models", t, func() {
		ch := &model.Channel{Models: "", ModelsAlias: ""}
		So(selectAutoModel(ch), ShouldEqual, "")
	})
}

func TestAutoDistribute(t *testing.T) {
	Convey("autoDistribute with 2 channels picks one round-robin and selects model", t, func() {
		channels := []*model.Channel{
			{Name: "A", Id: 1, Models: "gpt-4,gpt-3.5-turbo", ModelsAlias: "gpt4,gpt35"},
			{Name: "B", Id: 2, Models: "claude-3-opus", ModelsAlias: "claude3opus"},
		}
		ch, model, err := autoDistribute("autodist_test_1", channels)
		So(err, ShouldBeNil)
		So(ch, ShouldNotBeNil)
		// first call reads index 0 (before increment), returns channel A
		So(ch.Name, ShouldEqual, "A")
		So(ch.Id, ShouldEqual, 1)
		So(model, ShouldEqual, "gpt4")
	})

	Convey("autoDistribute with empty channels returns error", t, func() {
		_, _, err := autoDistribute("autodist_test_2", []*model.Channel{})
		So(err, ShouldNotBeNil)
	})
}

func TestNextAutoChannelRoundRobin(t *testing.T) {
	Convey("nextAutoChannel rotates through channels", t, func() {
		channels := []*model.Channel{
			{Name: "A", Id: 1},
			{Name: "B", Id: 2},
			{Name: "C", Id: 3},
		}
		group := "default"

		ch, idx := nextAutoChannel(group, channels)
		// reads index 0 before increment, returns A
		So(ch.Name, ShouldEqual, "A")
		So(idx, ShouldEqual, 0)

		ch, idx = nextAutoChannel(group, channels)
		// reads index 1 before increment, returns B
		So(ch.Name, ShouldEqual, "B")
		So(idx, ShouldEqual, 1)

		ch, idx = nextAutoChannel(group, channels)
		// reads index 2 before increment, returns C
		So(ch.Name, ShouldEqual, "C")
		So(idx, ShouldEqual, 2)

		ch, idx = nextAutoChannel(group, channels)
		// wraps around: reads index 0 before increment, returns A
		So(ch.Name, ShouldEqual, "A")
		So(idx, ShouldEqual, 0)
	})
}
