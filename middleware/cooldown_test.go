package middleware

import (
	"sync"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

func TestCooldownManagerCompositeKey(t *testing.T) {
	Convey("Put/IsCoolingDown 按 (channelId, model) 精确匹配", t, func() {
		cm := NewCooldownManager(60, nil, nil)
		cm.Put(1, "m1")

		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)
		// 同渠道不同模型互不影响
		So(cm.IsCoolingDown(1, "m2"), ShouldBeFalse)
		// 同模型不同渠道互不影响
		So(cm.IsCoolingDown(2, "m1"), ShouldBeFalse)
	})

	Convey("Put 空 model 退化为渠道级冷却", t, func() {
		cm := NewCooldownManager(60, nil, nil)
		cm.Put(1, "")

		So(cm.IsCoolingDown(1, ""), ShouldBeTrue)
		// 渠道级条目不直接影响具体模型的精确判定
		So(cm.IsCoolingDown(1, "m1"), ShouldBeFalse)
	})

	Convey("模型名归一化：两侧 TrimSpace 后命中同一 key", t, func() {
		cm := NewCooldownManager(60, nil, nil)
		cm.Put(1, " m1 ")
		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)
	})

	Convey("ResetChannel 清除该渠道所有模型的冷却条目", t, func() {
		cm := NewCooldownManager(60, nil, nil)
		cm.Put(1, "m1")
		cm.Put(1, "m2")
		cm.Put(1, "")
		cm.Put(2, "m1")

		cm.ResetChannel(1)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeFalse)
		So(cm.IsCoolingDown(1, "m2"), ShouldBeFalse)
		So(cm.IsCoolingDown(1, ""), ShouldBeFalse)
		// 其他渠道不受影响
		So(cm.IsCoolingDown(2, "m1"), ShouldBeTrue)
	})

	Convey("冷却过期后惰性删除不再命中", t, func() {
		cm := NewCooldownManager(0, nil, nil)
		cm.Put(1, "m1")
		So(cm.IsCoolingDown(1, "m1"), ShouldBeFalse)
	})
}

// fakeClock 用于在不依赖 time.Sleep 的情况下推进时间
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestManager 构造可注入时钟和固定阈值/窗口的冷却管理器
func newTestManager(cooldownSeconds, threshold, windowSeconds int) (*CooldownManager, *fakeClock) {
	clk := newFakeClock()
	cm := NewCooldownManager(
		cooldownSeconds,
		func() int { return threshold },
		func() int { return windowSeconds },
	)
	cm.now = clk.Now
	return cm, clk
}

func TestReportFailure(t *testing.T) {
	Convey("ReportFailure 权重 0 完全不计数不冷却", t, func() {
		cm, _ := newTestManager(60, 3, 120)
		cm.ReportFailure(1, "m1", 0)
		cm.ReportFailure(1, "m1", 0)
		cm.ReportFailure(1, "m1", 0)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeFalse)
	})

	Convey("ReportFailure 权重 1 需累计达到阈值才进入冷却，未达阈值前 IsCoolingDown 为 false", t, func() {
		cm, _ := newTestManager(60, 3, 120)
		cm.ReportFailure(1, "m1", 1)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeFalse)
		cm.ReportFailure(1, "m1", 1)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeFalse)
		cm.ReportFailure(1, "m1", 1)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)
	})

	Convey("ReportFailure 单次权重 2 加上后续权重 1 达到阈值直接触发冷却", t, func() {
		cm, _ := newTestManager(60, 3, 120)
		cm.ReportFailure(1, "m1", 2)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeFalse)
		cm.ReportFailure(1, "m1", 1)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)
	})

	Convey("ReportFailure 冷却触发后 failCount 清零，冷却解除后需重新累计才能再次触发", t, func() {
		cm, clk := newTestManager(60, 3, 120)
		cm.ReportFailure(1, "m1", 1)
		cm.ReportFailure(1, "m1", 1)
		cm.ReportFailure(1, "m1", 1)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)

		// 时间推进过冷却时长 → 冷却自然到期
		clk.Advance(61 * time.Second)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeFalse)

		// 此时 failCount 已清零，仅一次失败权重 1 不会再次进入冷却
		cm.ReportFailure(1, "m1", 1)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeFalse)
		// 再累计两次才达到阈值
		cm.ReportFailure(1, "m1", 1)
		cm.ReportFailure(1, "m1", 1)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)
	})

	Convey("ReportFailure 处于冷却中时再次失败不会刷新到期时间，仅重置计数", t, func() {
		cm, clk := newTestManager(60, 3, 120)
		cm.ReportFailure(1, "m1", 1)
		cm.ReportFailure(1, "m1", 1)
		cm.ReportFailure(1, "m1", 1)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)

		// 推进少量时间，仍在冷却中
		clk.Advance(5 * time.Second)
		cm.ReportFailure(1, "m1", 1)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)
		// 推进到接近到期边界前仍应冷却中（说明到期未被刷新）
		clk.Advance(54 * time.Second)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)
		// 再推进越过到期时间进入冷却解除
		clk.Advance(2 * time.Second)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeFalse)
	})

	Convey("ReportFailure 超出窗口则先清零再累加", t, func() {
		cm, clk := newTestManager(60, 3, 120)
		cm.ReportFailure(1, "m1", 1)
		cm.ReportFailure(1, "m1", 1)
		// 此时 failCount=2，尚未触发；推进时间超出窗口
		clk.Advance(121 * time.Second)
		cm.ReportFailure(1, "m1", 1)
		// 之前累计因超窗口清零，本次单次失败不应触发冷却
		So(cm.IsCoolingDown(1, "m1"), ShouldBeFalse)
		// 窗口内累计两次再加本次 = 3 → 触发
		cm.ReportFailure(1, "m1", 1)
		cm.ReportFailure(1, "m1", 1)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)
	})

	Convey("ReportFailure 窗口=0 时禁用窗口清零逻辑", t, func() {
		cm, clk := newTestManager(60, 3, 0)
		cm.ReportFailure(1, "m1", 1)
		cm.ReportFailure(1, "m1", 1)
		clk.Advance(1 * time.Hour)
		// 窗口禁用，前两次累计仍有效；本次再失败一次即可触发
		cm.ReportFailure(1, "m1", 1)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)
	})
}

func TestResetSuccess(t *testing.T) {
	Convey("ResetSuccess 清零计数但不影响正在冷却的条目", t, func() {
		cm, _ := newTestManager(60, 3, 120)
		cm.ReportFailure(1, "m1", 1)
		cm.ReportFailure(1, "m1", 1)
		cm.ResetSuccess(1, "m1")
		// failCount 已清零，单次失败不足以触发
		cm.ReportFailure(1, "m1", 1)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeFalse)

		// 已冷却状态下调用 ResetSuccess 不提前解除冷却
		cm.ReportFailure(1, "m1", 1)
		cm.ReportFailure(1, "m1", 1)
		cm.ReportFailure(1, "m1", 1)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)
		cm.ResetSuccess(1, "m1")
		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)
	})

	Convey("ResetSuccess 不存在的 key 是 no-op", t, func() {
		cm, _ := newTestManager(60, 3, 120)
		So(func() { cm.ResetSuccess(99, "nope") }, ShouldNotPanic)
	})
}

func TestCooldownManagerConfigIntegration(t *testing.T) {
	Convey("阈值/窗口通过 source 函数实时读取，模拟 DB 更新配置后生效", t, func() {
		threshold := 3
		window := 120
		clk := newFakeClock()
		cm := NewCooldownManager(
			60,
			func() int { return threshold },
			func() int { return window },
		)
		cm.now = clk.Now

		// 初始阈值 3：累计 3 次进入冷却
		cm.ReportFailure(1, "m1", 1)
		cm.ReportFailure(1, "m1", 1)
		cm.ReportFailure(1, "m1", 1)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)

		// 等冷却自然到期
		clk.Advance(61 * time.Second)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeFalse)

		// 模拟运营下调阈值到 2：单次失败 + 后续两次失败中任一次即可触发
		threshold = 2
		cm.ReportFailure(1, "m1", 1)
		cm.ReportFailure(1, "m1", 1)
		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)
	})
}