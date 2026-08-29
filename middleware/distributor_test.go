package middleware

import (
	"context"
	"testing"

	"github.com/gin-gonic/gin"
	. "github.com/smartystreets/goconvey/convey"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/model"
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
		ch, model, err := autoDistribute(context.Background(), "autodist_test_1", channels)
		So(err, ShouldBeNil)
		So(ch, ShouldNotBeNil)
		// first call reads index 0 (before increment), returns channel A
		So(ch.Name, ShouldEqual, "A")
		So(ch.Id, ShouldEqual, 1)
		So(model, ShouldEqual, "gpt4")
	})

	Convey("autoDistribute with empty channels returns error", t, func() {
		_, _, err := autoDistribute(context.Background(), "autodist_test_2", []*model.Channel{})
		So(err, ShouldNotBeNil)
	})
}

func clearAffinity() {
	AffinityGlobal.Remove(999, "gpt4turbo")
	AffinityGlobal.Remove(999, "gpt35turbo")
	// deepseek 系列供 canonical 等价组用例使用，affinity 以原始请求模型名为 key
	AffinityGlobal.Remove(999, "deepseek-v4-flash")
	AffinityGlobal.Remove(999, "deepseek-v4-flash-0731")
}

func TestNonAutoDistributeNoAffinity(t *testing.T) {
	Convey("nonAutoDistribute with no affinity falls back to weighted select", t, func() {
		clearAffinity()
		channels := []*model.Channel{
			{Name: "A", Id: 1, ModelsAlias: "gpt4turbo", Models: "gpt-4-turbo"},
		}

		ch, model, err := nonAutoDistribute(context.Background(), 999, "gpt4turbo", channels)
		So(err, ShouldBeNil)
		So(ch.Name, ShouldEqual, "A")
		So(ch.Id, ShouldEqual, 1)
		So(model, ShouldEqual, "gpt-4-turbo")
	})

	Convey("nonAutoDistribute with no matched channels returns error", t, func() {
		clearAffinity()
		channels := []*model.Channel{
			{Name: "A", Id: 1, ModelsAlias: "gpt4turbo"},
		}

		_, _, err := nonAutoDistribute(context.Background(), 999, "nonexistent-model", channels)
		So(err, ShouldNotBeNil)
	})

	Convey("nonAutoDistribute with empty channel list returns error", t, func() {
		clearAffinity()
		_, _, err := nonAutoDistribute(context.Background(), 999, "gpt4turbo", []*model.Channel{})
		So(err, ShouldNotBeNil)
	})
}

func TestNonAutoDistributeWithAffinity(t *testing.T) {
	Convey("nonAutoDistribute respects affinity when channel is in matched set", t, func() {
		clearAffinity()
		AffinityGlobal.Set(999, "gpt4turbo", 2)
		channels := []*model.Channel{
			{Name: "A", Id: 1, ModelsAlias: "gpt4turbo", Models: "gpt-4-turbo"},
			{Name: "B", Id: 2, ModelsAlias: "gpt4turbo", Models: "gpt-4-turbo"},
		}

		ch, model, err := nonAutoDistribute(context.Background(), 999, "gpt4turbo", channels)
		So(err, ShouldBeNil)
		So(ch.Name, ShouldEqual, "B")
		So(ch.Id, ShouldEqual, 2)
		So(model, ShouldEqual, "gpt-4-turbo")
	})

	Convey("nonAutoDistribute falls back to weighted when affinity channel missing", t, func() {
		clearAffinity()
		AffinityGlobal.Set(999, "gpt4turbo", 99) // affinity points to channel not in the list
		channels := []*model.Channel{
			{Name: "A", Id: 1, ModelsAlias: "gpt4turbo", Models: "gpt-4-turbo"},
		}

		ch, model, err := nonAutoDistribute(context.Background(), 999, "gpt4turbo", channels)
		So(err, ShouldBeNil)
		So(ch.Name, ShouldEqual, "A")
		So(ch.Id, ShouldEqual, 1)
		So(model, ShouldEqual, "gpt-4-turbo")
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

func TestMatchChannelsByAliasCanonical(t *testing.T) {
	// 用后清理，避免污染其他用例
	defer model.ResetCanonicalAliasForTest()

	Convey("canonical equivalent models match both channels from either direction", t, func() {
		model.SetCanonicalAliasForTest("deepseekv4flash", "deepseekv4flash0731")

		channels := []*model.Channel{
			{Name: "A", Id: 1, Models: "deepseek-v4-flash-0731", ModelsAlias: "deepseekv4flash0731"},
			{Name: "B", Id: 2, Models: "deepseek-v4-flash", ModelsAlias: "deepseekv4flash"},
		}

		matchedByShort, _ := matchChannelsByAlias("deepseek-v4-flash", channels)
		So(len(matchedByShort), ShouldEqual, 2)

		matchedByLong, _ := matchChannelsByAlias("deepseek-v4-flash-0731", channels)
		So(len(matchedByLong), ShouldEqual, 2)
	})

	Convey("nonAutoDistribute returns each channel's real model name via canonical match", t, func() {
		model.SetCanonicalAliasForTest("deepseekv4flash", "deepseekv4flash0731")

		clearAffinity()
		chA := []*model.Channel{
			{Name: "A", Id: 1, Models: "deepseek-v4-flash-0731", ModelsAlias: "deepseekv4flash0731"},
		}
		_, longName, err := nonAutoDistribute(context.Background(), 999, "deepseek-v4-flash", chA)
		So(err, ShouldBeNil)
		So(longName, ShouldEqual, "deepseek-v4-flash-0731")

		clearAffinity()
		chB := []*model.Channel{
			{Name: "B", Id: 2, Models: "deepseek-v4-flash", ModelsAlias: "deepseekv4flash"},
		}
		_, shortName, err := nonAutoDistribute(context.Background(), 999, "deepseek-v4-flash-0731", chB)
		So(err, ShouldBeNil)
		So(shortName, ShouldEqual, "deepseek-v4-flash")
	})
}

func TestCanonicalizeSimplifiedName(t *testing.T) {
	Convey("canonicalize maps configured member to canonical name", t, func() {
		defer model.ResetCanonicalAliasForTest()
		model.SetCanonicalAliasForTest("deepseekv4flash", "deepseekv4flash0731")

		So(model.CanonicalizeSimplifiedName("deepseekv4flash"), ShouldEqual, "deepseekv4flash0731")
	})

	Convey("canonicalize returns input unchanged when not configured", t, func() {
		defer model.ResetCanonicalAliasForTest()

		So(model.CanonicalizeSimplifiedName("gpt4turbo"), ShouldEqual, "gpt4turbo")
	})
}

func TestNonAutoDistributeCanonicalBeforePrefix(t *testing.T) {
	Convey("nonAutoDistribute resolves via canonical match before prefix match", t, func() {
		defer model.ResetCanonicalAliasForTest()
		model.SetCanonicalAliasForTest("deepseekv4flash", "deepseekv4flash0731")
		clearAffinity()

		// 别名 idx=0 满足前缀段、idx=1 满足 canonical 段：
		// 若前缀段抢先应返回 models[0]，canonical 段先命中则返回 models[1]
		channels := []*model.Channel{
			{Name: "C", Id: 3, Models: "model-x,deepseek-v4-flash-0731", ModelsAlias: "deepseekv4flashx,deepseekv4flash0731"},
		}

		ch, modelName, err := nonAutoDistribute(context.Background(), 999, "deepseek-v4-flash", channels)
		So(err, ShouldBeNil)
		So(ch.Id, ShouldEqual, 3)
		So(modelName, ShouldEqual, "deepseek-v4-flash-0731")
	})
}
