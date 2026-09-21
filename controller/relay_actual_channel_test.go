package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/middleware"
	"github.com/pai801/myapi/relay/model"
	. "github.com/smartystreets/goconvey/convey"
)

// TestResolveActualChannelId 锁定归因渠道选择：有 ActualChannelId（sticky 覆盖）时按实际渠道，
// 否则退化为选路渠道。
func TestResolveActualChannelId(t *testing.T) {
	Convey("resolveActualChannelId 按实际服务渠道归因", t, func() {
		Convey("无覆盖（未记录 ActualChannelId）时退化为选路渠道", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.ChannelId, 100)
			So(resolveActualChannelId(c, 100), ShouldEqual, 100)
		})

		Convey("sticky 覆盖时以实际渠道为准", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.ChannelId, 100)
			c.Set(ctxkey.ActualChannelId, 200)
			So(resolveActualChannelId(c, 100), ShouldEqual, 200)
		})

		Convey("ActualChannelId 与选路渠道相等时退化为选路渠道", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.ActualChannelId, 100)
			So(resolveActualChannelId(c, 100), ShouldEqual, 100)
		})

		Convey("ActualChannelId<=0 不生效", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.ActualChannelId, 0)
			So(resolveActualChannelId(c, 100), ShouldEqual, 100)
		})
	})
}

// TestResolveActualChannelName 锁定失败日志渠道名选择：有 ActualChannelName（sticky 覆盖）时
// 返回实际渠道名 B，否则退化为 fallback（选路渠道名 A）。
func TestResolveActualChannelName(t *testing.T) {
	Convey("resolveActualChannelName 按实际服务渠道名归因", t, func() {
		Convey("无覆盖（未记录 ActualChannelName）时退化为选路渠道名", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.ChannelName, "channel-A")
			So(resolveActualChannelName(c, c.GetString(ctxkey.ChannelName)), ShouldEqual, "channel-A")
		})

		Convey("sticky 覆盖时以实际渠道名为准", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.ChannelName, "channel-A")
			c.Set(ctxkey.ActualChannelName, "channel-B")
			So(resolveActualChannelName(c, c.GetString(ctxkey.ChannelName)), ShouldEqual, "channel-B")
		})

		Convey("ActualChannelName 为空串时退化为选路渠道名", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.ChannelName, "channel-A")
			c.Set(ctxkey.ActualChannelName, "")
			So(resolveActualChannelName(c, c.GetString(ctxkey.ChannelName)), ShouldEqual, "channel-A")
		})
	})
}

// TestFailurePathChannelNameUsesActualChannel 是 P2-1 的硬指标：sticky 覆盖后，失败路径
// 取用的 channelName 必须是「实际服务渠道 B 的名字」，而不是选路渠道 A 的名字。
// 直接复刻 controller/relay.go:98/141 的表达式 resolveActualChannelName(c, c.GetString(ctxkey.ChannelName))。
func TestFailurePathChannelNameUsesActualChannel(t *testing.T) {
	Convey("失败路径 channelName 按实际服务渠道", t, func() {
		const selected = 100
		const actual = 200

		Convey("sticky 覆盖：失败路径拿到 B 的名字（硬断言）", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.ChannelId, selected)
			c.Set(ctxkey.ChannelName, "channel-A")
			c.Set(ctxkey.ActualChannelId, actual)
			c.Set(ctxkey.ActualChannelName, "channel-B")

			got := resolveActualChannelName(c, c.GetString(ctxkey.ChannelName))
			So(got, ShouldEqual, "channel-B")
			So(resolveActualChannelId(c, selected), ShouldEqual, actual)
		})

		Convey("无覆盖：失败路径拿到选路渠道名（与改动前等价）", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.ChannelId, selected)
			c.Set(ctxkey.ChannelName, "channel-A")

			got := resolveActualChannelName(c, c.GetString(ctxkey.ChannelName))
			So(got, ShouldEqual, "channel-A")
			So(resolveActualChannelId(c, selected), ShouldEqual, selected)
		})
	})
}

// TestRecordSuccessAttributionAffinityUsesActualChannel 是本次修复的硬指标：
// sticky 覆盖后，亲和必须记录「实际服务渠道 B」，而不是选路渠道 A。
// 同时锁定无覆盖场景与改动前等价（亲和记选路渠道）。
func TestRecordSuccessAttributionAffinityUsesActualChannel(t *testing.T) {
	Convey("成功归因的亲和写入按实际服务渠道", t, func() {
		const selected = 100
		const actual = 200
		modelName := "gpt-4o-affinity-attr-test"
		scope := middleware.AffinityScope{UserID: 4242, Group: "default", SessionID: "sess-affinity-attr"}
		keys := scope.KeysToSet(modelName)
		defer func() {
			for _, key := range keys {
				middleware.AffinityGlobal.Remove(key)
			}
		}()

		Convey("sticky 覆盖：亲和记录实际渠道 B", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.ChannelId, selected)
			c.Set(ctxkey.ActualChannelId, actual)
			c.Set(ctxkey.SuggestedModel, modelName)

			got := recordSuccessAttribution(c, selected, modelName, scope)
			So(got, ShouldEqual, actual)
			for _, key := range keys {
				ch, _, ok := middleware.AffinityGlobal.Get([]middleware.AffinityKey{key})
				So(ok, ShouldBeTrue)
				So(ch, ShouldEqual, actual)
			}
		})

		Convey("无覆盖：亲和记录选路渠道（与改动前等价）", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.ChannelId, selected)
			c.Set(ctxkey.SuggestedModel, modelName)

			got := recordSuccessAttribution(c, selected, modelName, scope)
			So(got, ShouldEqual, selected)
			for _, key := range keys {
				ch, _, ok := middleware.AffinityGlobal.Get([]middleware.AffinityKey{key})
				So(ok, ShouldBeTrue)
				So(ch, ShouldEqual, selected)
			}
		})
	})
}

// TestRecordSuccessAttributionCooldownUsesActualChannel 直接断言冷却归因落到实际渠道：
// 借 failCount 累计 + 阈值判别 ResetSuccess 命中的渠道。
// 先给实际渠道 B 预累计 2 次（阈值 3，未冷却）；成功归因应清零 B。
// 之后若再 +1：命中 B（修复后）→ 计数 1（<3，不冷却）；误命中 A（bug）→ B 仍为 2，+1=3 触发冷却。
func TestRecordSuccessAttributionCooldownUsesActualChannel(t *testing.T) {
	Convey("成功归因的冷却清零按实际服务渠道", t, func() {
		oldThreshold := config.ChannelCooldownErrorThreshold
		oldWindow := config.ChannelCooldownErrorWindowSeconds
		config.ChannelCooldownErrorThreshold = 3
		config.ChannelCooldownErrorWindowSeconds = 120
		defer func() {
			config.ChannelCooldownErrorThreshold = oldThreshold
			config.ChannelCooldownErrorWindowSeconds = oldWindow
		}()

		const selected = 101
		const actual = 201
		model := "gpt-4o-cooldown-attr-test"
		scope := middleware.AffinityScope{UserID: 4343, Group: "default", SessionID: "sess-cooldown-attr"}

		middleware.CooldownGlobal.ResetChannel(selected)
		middleware.CooldownGlobal.ResetChannel(actual)
		defer func() {
			middleware.CooldownGlobal.ResetChannel(selected)
			middleware.CooldownGlobal.ResetChannel(actual)
			for _, key := range scope.KeysToSet(model) {
				middleware.AffinityGlobal.Remove(key)
			}
		}()

		c, _ := gin.CreateTestContext(nil)
		c.Set(ctxkey.ChannelId, selected)
		c.Set(ctxkey.ActualChannelId, actual)
		c.Set(ctxkey.SuggestedModel, model)

		middleware.CooldownGlobal.ReportFailure(actual, model, 1)
		middleware.CooldownGlobal.ReportFailure(actual, model, 1)
		So(middleware.CooldownGlobal.IsCoolingDown(actual, model), ShouldBeFalse)

		recordSuccessAttribution(c, selected, model, scope)

		middleware.CooldownGlobal.ReportFailure(actual, model, 1)
		So(middleware.CooldownGlobal.IsCoolingDown(actual, model), ShouldBeFalse)
	})
}

// TestFailureAttributionTargetsGivenChannel 证明失败归因目标 processChannelRelayError 会把冷却记到
// 传入的渠道上。失败调用点（controller/relay.go）传入的是 resolveActualChannelId(c, channelId)，
// 故其“按实际渠道归因”由本用例 + TestResolveActualChannelId 共同覆盖。
func TestFailureAttributionTargetsGivenChannel(t *testing.T) {
	Convey("processChannelRelayError 按传入渠道归因冷却", t, func() {
		oldThreshold := config.ChannelCooldownErrorThreshold
		oldWindow := config.ChannelCooldownErrorWindowSeconds
		config.ChannelCooldownErrorThreshold = 1
		config.ChannelCooldownErrorWindowSeconds = 120
		defer func() {
			config.ChannelCooldownErrorThreshold = oldThreshold
			config.ChannelCooldownErrorWindowSeconds = oldWindow
		}()

		const selected = 102
		const actual = 202
		modelName := "gpt-4o-fail-attr-test"

		middleware.CooldownGlobal.ResetChannel(selected)
		middleware.CooldownGlobal.ResetChannel(actual)
		defer func() {
			middleware.CooldownGlobal.ResetChannel(selected)
			middleware.CooldownGlobal.ResetChannel(actual)
		}()

		bizErr := model.ErrorWithStatusCode{
			StatusCode: 500,
			Error:      model.Error{Message: "upstream timeout", Type: "server_error"},
		}

		processChannelRelayError(context.Background(), 1, actual, "test-ch", modelName, bizErr)

		So(middleware.CooldownGlobal.IsCoolingDown(actual, modelName), ShouldBeTrue)
		So(middleware.CooldownGlobal.IsCoolingDown(selected, modelName), ShouldBeFalse)
	})
}

// TestBuildFailureLogChannelIdUsesActualChannel 是 P2 的硬指标：失败日志的 ChannelId 必须与
// channelName 同口径 —— sticky 覆盖后记「实际服务渠道 B」，而不是选路渠道 A（否则同一失败
// 会被记到「A 的 id + B 的名字」两个渠道上）。无覆盖时退化为选路渠道，与改动前等价。
func TestBuildFailureLogChannelIdUsesActualChannel(t *testing.T) {
	Convey("失败日志 ChannelId 按实际服务渠道", t, func() {
		const selected = 100
		const actual = 200

		Convey("sticky 覆盖：ChannelId 记为实际渠道 B", func() {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
			c.Set(ctxkey.Id, 7)
			c.Set(ctxkey.ChannelId, selected)
			c.Set(ctxkey.ActualChannelId, actual)
			c.Set(ctxkey.RequestModel, "gpt-4o")

			bizErr := &model.ErrorWithStatusCode{
				StatusCode: 500,
				Error:      model.Error{Message: "boom", Type: "server_error"},
			}
			log := buildFailureLog(c, bizErr, "B-name")
			So(log.ChannelId, ShouldEqual, actual)
			So(log.ChannelName, ShouldEqual, "B-name")
		})

		Convey("无覆盖：ChannelId 记为选路渠道 A（与改动前等价）", func() {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
			c.Set(ctxkey.ChannelId, selected)

			bizErr := &model.ErrorWithStatusCode{
				StatusCode: 500,
				Error:      model.Error{Message: "boom", Type: "server_error"},
			}
			log := buildFailureLog(c, bizErr, "A-name")
			So(log.ChannelId, ShouldEqual, selected)
		})
	})
}
