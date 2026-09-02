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

		matched, _, _ := matchChannelsByAlias("gpt4turbo", channels)
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

		exact, prefix, _ := matchChannelsByAlias("gpt-4", channels)
		// 请求简化名 "gpt4" 不等于任何 alias 的精确形式 → exact 集为空
		So(len(exact), ShouldEqual, 0)
		// 两渠道 alias 均以 "gpt4" 开头 → prefix 集应有 2 个
		So(len(prefix), ShouldEqual, 2)
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
	Convey("selectAutoModel 从渠道实际模型集随机选择，alias 不参与 auto 选模型：多轮采样均落在未冷却集合内", t, func() {
		ch := &model.Channel{Models: "gpt-4,gpt-3.5-turbo", ModelsAlias: "gpt4,gpt35"}
		for i := 0; i < 20; i++ {
			So(selectAutoModel(ch), ShouldBeIn, []string{"gpt-4", "gpt-3.5-turbo"})
		}
	})

	Convey("selectAutoModel 无 alias 时同样从实际模型集随机选择", t, func() {
		ch := &model.Channel{Models: "gpt-4,gpt-3.5-turbo"}
		for i := 0; i < 20; i++ {
			So(selectAutoModel(ch), ShouldBeIn, []string{"gpt-4", "gpt-3.5-turbo"})
		}
	})

	Convey("selectAutoModel returns empty string when no models", t, func() {
		ch := &model.Channel{Models: "", ModelsAlias: ""}
		So(selectAutoModel(ch), ShouldEqual, "")
	})
}

func TestAutoDistribute(t *testing.T) {
	Convey("autoDistribute with 2 channels picks one round-robin and selects model from its actual model set", t, func() {
		channels := []*model.Channel{
			{Name: "A", Id: 1, Models: "gpt-4,gpt-3.5-turbo", ModelsAlias: "gpt4,gpt35"},
			{Name: "B", Id: 2, Models: "claude-3-opus", ModelsAlias: "claude3opus"},
		}
		ch, modelName, err := autoDistribute(context.Background(), "autodist_test_1", channels)
		So(err, ShouldBeNil)
		So(ch, ShouldNotBeNil)
		// first call reads index 0 (before increment), returns channel A
		So(ch.Name, ShouldEqual, "A")
		So(ch.Id, ShouldEqual, 1)
		// auto 模式从被选中渠道的实际模型集随机取实际模型名，而非 alias
		So(modelName, ShouldBeIn, []string{"gpt-4", "gpt-3.5-turbo"})
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

		matchedByShort, _, _ := matchChannelsByAlias("deepseek-v4-flash", channels)
		So(len(matchedByShort), ShouldEqual, 2)

		matchedByLong, _, _ := matchChannelsByAlias("deepseek-v4-flash-0731", channels)
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

// TestMatchChannelsByAliasMergesExactAndPrefix 验证新分发语义：matchChannelsByAlias
// 返回 (exact, prefix) 两组；nonAutoDistribute 仅在 exact 集内 weighted 选，exact 为空
// 才在 prefix 集选；亲和命中允许命中 prefix-only 渠道（亲和兜底）。
func TestMatchChannelsByAliasMergesExactAndPrefix(t *testing.T) {
	defer model.ResetCanonicalAliasForTest()

	Convey("matchChannelsByAlias 把渠道分到 exact / prefix 两组，且去重", t, func() {
		// 请求简化名 "gpt4turbovision"：id=10 alias 完全相等属 exact；id=20/30 alias 以其开头属 prefix
		channels := []*model.Channel{
			{Name: "EXACT", Id: 10, Models: "gpt-4-turbo-vision", ModelsAlias: "gpt4turbovision"},
			{Name: "PREFIX_vision_pro", Id: 20, Models: "gpt-4-turbo-vision-pro", ModelsAlias: "gpt4turbovisionpro"},
			{Name: "PREFIX_only", Id: 30, Models: "gpt-4-turbo-vision-mini", ModelsAlias: "gpt4turbovisionmini"},
			{Name: "UNRELATED", Id: 40, Models: "claude-3-opus", ModelsAlias: "claude3opus"},
		}

		exact, prefix, _ := matchChannelsByAlias("gpt-4-turbo-vision", channels)

		// exact 集只有 id=10；prefix 集有 id=20/30；UNRELATED 两边都不在
		So(len(exact), ShouldEqual, 1)
		So(exact[0].Id, ShouldEqual, 10)
		So(len(prefix), ShouldEqual, 2)
		prefixSet := map[int]bool{prefix[0].Id: true, prefix[1].Id: true}
		So(prefixSet[20], ShouldBeTrue)
		So(prefixSet[30], ShouldBeTrue)
	})

	Convey("nonAutoDistribute 在 exact 集内选，不被 prefix-only 高优先级渠道稀释", t, func() {
		clearAffinity()
		// id=10 (exact) Priority=10，id=20 (prefix-only) Priority=100；
		// 若 prefix 进候选池 weighted 选，高优先级的 id=20 会被选中并把变体模型发给上游。
		pExact := int64(10)
		pPrefix := int64(100)
		channels := []*model.Channel{
			{Name: "EXACT", Id: 10, Models: "gpt-4-turbo-vision", ModelsAlias: "gpt4turbovision", Priority: &pExact},
			{Name: "PREFIX_HIGHER", Id: 20, Models: "gpt-4-turbo-vision-pro", ModelsAlias: "gpt4turbovisionpro", Priority: &pPrefix},
		}

		ch, modelName, err := nonAutoDistribute(context.Background(), 999, "gpt-4-turbo-vision", channels)
		So(err, ShouldBeNil)
		// exact 集优先：必选 id=10，不被 prefix-only 渠道抢占
		So(ch.Id, ShouldEqual, 10)
		So(modelName, ShouldEqual, "gpt-4-turbo-vision")
	})

	Convey("亲和命中 prefix-only 渠道时仍能选中（亲和兜底）", t, func() {
		clearAffinity()
		// 候选里没有 exact，只有 prefix-only 渠道 #72；亲和指向它也能命中
		channels := []*model.Channel{
			{Name: "PREFIX_ONLY", Id: 72, Models: "gpt-4-turbo-vision-pro", ModelsAlias: "gpt4turbovisionpro"},
		}
		AffinityGlobal.Set(999, "gpt-4-turbo-vision", 72)

		ch, modelName, err := nonAutoDistribute(context.Background(), 999, "gpt-4-turbo-vision", channels)
		So(err, ShouldBeNil)
		So(ch.Id, ShouldEqual, 72)
		So(modelName, ShouldEqual, "gpt-4-turbo-vision-pro")
	})

	Convey("exact 集为空时 prefix 集可作 weighted 兜底", t, func() {
		clearAffinity()
		// 没有 exact 命中；prefix-only 渠道 #20 唯一候选
		channels := []*model.Channel{
			{Name: "PREFIX_ONLY", Id: 20, Models: "gpt-4-turbo-vision-pro", ModelsAlias: "gpt4turbovisionpro"},
		}

		ch, modelName, err := nonAutoDistribute(context.Background(), 999, "gpt-4-turbo-vision", channels)
		So(err, ShouldBeNil)
		So(ch.Id, ShouldEqual, 20)
		So(modelName, ShouldEqual, "gpt-4-turbo-vision-pro")
	})

	Convey("exact + prefix 同时命中时同渠道只算一次且归入 exact", t, func() {
		channels := []*model.Channel{
			// alias "gpt4turbovision" 既精确等于请求、又以请求为前缀，应只入 exact
			{Name: "DUP", Id: 50, Models: "gpt-4-turbo-vision", ModelsAlias: "gpt4turbovision"},
		}
		exact, prefix, _ := matchChannelsByAlias("gpt-4-turbo-vision", channels)
		So(len(exact), ShouldEqual, 1)
		So(exact[0].Id, ShouldEqual, 50)
		So(len(prefix), ShouldEqual, 0)
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

func TestFilterCoolingChannels(t *testing.T) {
	Convey("非 auto：渠道 A 模型 m1 冷却时请求 m1 剔除 A，请求 m2 保留", t, func() {
		channels := []*model.Channel{
			{Name: "A", Id: 901, Models: "m1,m2", ModelsAlias: "m1,m2"},
			{Name: "B", Id: 902, Models: "m1,m2", ModelsAlias: "m1,m2"},
		}
		CooldownGlobal.Put(901, "m1")
		defer CooldownGlobal.ResetChannel(901)

		// 请求 m1：A 的实际服务模型 m1 冷却 → 剔除 A
		filtered := filterCoolingChannels(channels, "m1")
		So(len(filtered), ShouldEqual, 1)
		So(filtered[0].Id, ShouldEqual, 902)

		// 请求 m2：A 的 m2 未冷却 → 保留 A
		filtered = filterCoolingChannels(channels, "m2")
		So(len(filtered), ShouldEqual, 2)
	})

	Convey("非 auto：渠道不支持请求模型时不因冷却剔除", t, func() {
		channels := []*model.Channel{
			{Name: "A", Id: 903, Models: "m1", ModelsAlias: "m1"},
		}
		CooldownGlobal.Put(903, "m1")
		defer CooldownGlobal.ResetChannel(903)

		filtered := filterCoolingChannels(channels, "other-model")
		So(len(filtered), ShouldEqual, 1)
	})

	Convey("非 auto：canonical 等价别名按解析后的实际模型命中冷却", t, func() {
		defer model.ResetCanonicalAliasForTest()
		model.SetCanonicalAliasForTest("deepseekv4flash", "deepseekv4flash0731")

		channels := []*model.Channel{
			{Name: "A", Id: 904, Models: "deepseek-v4-flash-0731", ModelsAlias: "deepseekv4flash0731"},
		}
		CooldownGlobal.Put(904, "deepseek-v4-flash-0731")
		defer CooldownGlobal.ResetChannel(904)

		// 请求 deepseek-v4-flash 经等价匹配解析到实际模型 deepseek-v4-flash-0731 → 冷却剔除
		filtered := filterCoolingChannels(channels, "deepseek-v4-flash")
		So(len(filtered), ShouldEqual, 0)
	})

	Convey("auto：渠道部分模型冷却时保留，全部冷却时剔除", t, func() {
		channels := []*model.Channel{
			{Name: "A", Id: 905, Models: "m1,m2"},
			{Name: "B", Id: 906, Models: "m3"},
		}
		CooldownGlobal.Put(905, "m1")
		defer CooldownGlobal.ResetChannel(905)

		// A 仅 m1 冷却 → 保留
		filtered := filterCoolingChannels(channels, "auto")
		So(len(filtered), ShouldEqual, 2)

		// A 全部冷却 → 剔除
		CooldownGlobal.Put(905, "m2")
		filtered = filterCoolingChannels(channels, "auto")
		So(len(filtered), ShouldEqual, 1)
		So(filtered[0].Id, ShouldEqual, 906)
	})

	Convey("auto：渠道级冷却条目（空模型）剔除配置了模型的渠道", t, func() {
		channels := []*model.Channel{
			{Name: "A", Id: 910, Models: "m1,m2"},
		}
		CooldownGlobal.Put(910, "")
		defer CooldownGlobal.ResetChannel(910)

		filtered := filterCoolingChannels(channels, "auto")
		So(len(filtered), ShouldEqual, 0)
	})
}

func TestSelectAutoModelSkipsCooling(t *testing.T) {
	Convey("selectAutoModel 跳过冷却中的模型", t, func() {
		ch := &model.Channel{Id: 907, Models: "m1,m2"}
		CooldownGlobal.Put(907, "m1")
		defer CooldownGlobal.ResetChannel(907)

		// 多次采样应始终落在未冷却的 m2 上
		for i := 0; i < 20; i++ {
			So(selectAutoModel(ch), ShouldEqual, "m2")
		}
	})
}

func TestResolveSpecificChannelModel(t *testing.T) {
	Convey("指定渠道别名请求解析为实际模型，失败冷却后过滤侧按实际模型命中", t, func() {
		ch := &model.Channel{Id: 908, Models: "gpt-4-turbo", ModelsAlias: "gpt4turbo"}

		resolved := resolveSpecificChannelModel(ch, "gpt4turbo")
		So(resolved, ShouldEqual, "gpt-4-turbo")

		// 按解析结果 Put，过滤侧用原始别名请求应命中同一实际模型并剔除渠道
		CooldownGlobal.Put(908, resolved)
		defer CooldownGlobal.ResetChannel(908)
		filtered := filterCoolingChannels([]*model.Channel{ch}, "gpt4turbo")
		So(len(filtered), ShouldEqual, 0)
	})

	Convey("指定渠道不支持请求模型时退化为原始请求模型", t, func() {
		ch := &model.Channel{Id: 909, Models: "gpt-4-turbo", ModelsAlias: "gpt4turbo"}
		So(resolveSpecificChannelModel(ch, "other-model"), ShouldEqual, "other-model")
	})

	Convey("空 requestModel 退化为原始请求模型，不因空前缀误命中首个模型", t, func() {
		ch := &model.Channel{Id: 911, Models: "m1,m2", ModelsAlias: "m1,m2"}
		So(resolveSpecificChannelModel(ch, ""), ShouldEqual, "")
	})
}
