package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/middleware"
	dbmodel "github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay/model"
	. "github.com/smartystreets/goconvey/convey"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
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

// TestResolveLastFailedChannelId 锁定重试剔除渠道的选择口径：与归因同源，sticky 覆盖时剔除
// 实际服务渠道 B，否则退化为选路渠道 A。
func TestResolveLastFailedChannelId(t *testing.T) {
	Convey("resolveLastFailedChannelId 按实际服务渠道剔除", t, func() {
		Convey("无覆盖（未记录 ActualChannelId）时剔除选路渠道", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.ChannelId, 100)
			So(resolveLastFailedChannelId(c, 100), ShouldEqual, 100)
		})

		Convey("sticky 覆盖时剔除实际渠道 B", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.ChannelId, 100)
			c.Set(ctxkey.ActualChannelId, 200)
			So(resolveLastFailedChannelId(c, 100), ShouldEqual, 200)
		})

		Convey("ActualChannelId<=0 时退化为选路渠道", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.ActualChannelId, 0)
			So(resolveLastFailedChannelId(c, 100), ShouldEqual, 100)
		})

		Convey("ActualChannelId 与选路渠道相等时退化为选路渠道", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.ActualChannelId, 100)
			So(resolveLastFailedChannelId(c, 100), ShouldEqual, 100)
		})
	})
}

// TestRetryExclusionUsesActualChannel 锁定口径分裂修复的硬指标：sticky 覆盖后重试剔除的必须是
// 实际服务渠道 B（而非选路渠道 A），并顺带验证 middleware.SelectChannel 的剔除语义确实生效。
// 用临时 SQLite 库 + MemoryCacheEnabled=false（走 GetChannelsByGroup 直查，不污染全局渠道缓存）。
func TestRetryExclusionUsesActualChannel(t *testing.T) {
	oldMem := config.MemoryCacheEnabled
	oldSQLite := common.UsingSQLite
	oldDB := dbmodel.DB
	config.MemoryCacheEnabled = false
	common.UsingSQLite = true
	defer func() {
		config.MemoryCacheEnabled = oldMem
		common.UsingSQLite = oldSQLite
		dbmodel.DB = oldDB
	}()

	var err error
	dbmodel.DB, err = gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	if err := dbmodel.DB.AutoMigrate(&dbmodel.Channel{}, &dbmodel.Ability{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	const group = "retry-excl-group"
	// ModelsAlias 取请求模型名去分隔符后的形式，确保 SimplifyModelName 归一化后精确命中
	const requestModel = "retry-excl-model"
	const requestAlias = "retryexclmodel"
	seed := func(id int, name string) {
		ch := &dbmodel.Channel{
			Id: id, Name: name, Status: dbmodel.ChannelStatusEnabled,
			Group: group, Models: requestModel, ModelsAlias: requestAlias, Type: 1, Key: "sk-test",
		}
		if err := dbmodel.DB.Create(ch).Error; err != nil {
			t.Fatalf("seed channel %d: %v", id, err)
		}
		ab := &dbmodel.Ability{Group: group, Model: requestModel, ChannelId: id, Enabled: true}
		if err := dbmodel.DB.Create(ab).Error; err != nil {
			t.Fatalf("seed ability %d: %v", id, err)
		}
	}

	// 用隔离的渠道 id，避免与其它用例的冷却状态串扰
	const selected = 7001
	const actual = 7002
	seed(selected, "channel-A")
	seed(actual, "channel-B")
	defer func() {
		middleware.CooldownGlobal.ResetChannel(selected)
		middleware.CooldownGlobal.ResetChannel(actual)
	}()

	scope := middleware.AffinityScope{UserID: 999, Group: group}

	Convey("sticky 覆盖：剔除的是实际渠道 B", t, func() {
		c, _ := gin.CreateTestContext(nil)
		c.Set(ctxkey.ChannelId, selected)
		c.Set(ctxkey.ActualChannelId, actual)
		So(resolveLastFailedChannelId(c, selected), ShouldEqual, actual)

		// 剔除 B 后 A 仍在候选集，选路应落到 A（不会选回真正失败的 B）
		ch, _, err := middleware.SelectChannel(context.Background(), group, requestModel, actual, scope)
		So(err, ShouldBeNil)
		So(ch, ShouldNotBeNil)
		So(ch.Id, ShouldEqual, selected)
	})

	Convey("B 是唯一候选时剔除 B 使选路返回 error（证明剔除语义生效）", t, func() {
		// 清掉 A 的 ability，使 B 成为唯一候选
		if err := dbmodel.DB.Where("channel_id = ?", selected).Delete(&dbmodel.Ability{}).Error; err != nil {
			t.Fatalf("delete ability: %v", err)
		}
		_, _, err := middleware.SelectChannel(context.Background(), group, requestModel, actual, scope)
		So(err, ShouldNotBeNil)
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
