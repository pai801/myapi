package middleware

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestCooldownManagerCompositeKey(t *testing.T) {
	Convey("Put/IsCoolingDown 按 (channelId, model) 精确匹配", t, func() {
		cm := NewCooldownManager(60)
		cm.Put(1, "m1")

		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)
		// 同渠道不同模型互不影响
		So(cm.IsCoolingDown(1, "m2"), ShouldBeFalse)
		// 同模型不同渠道互不影响
		So(cm.IsCoolingDown(2, "m1"), ShouldBeFalse)
	})

	Convey("Put 空 model 退化为渠道级冷却", t, func() {
		cm := NewCooldownManager(60)
		cm.Put(1, "")

		So(cm.IsCoolingDown(1, ""), ShouldBeTrue)
		// 渠道级条目不直接影响具体模型的精确判定
		So(cm.IsCoolingDown(1, "m1"), ShouldBeFalse)
	})

	Convey("模型名归一化：两侧 TrimSpace 后命中同一 key", t, func() {
		cm := NewCooldownManager(60)
		cm.Put(1, " m1 ")
		So(cm.IsCoolingDown(1, "m1"), ShouldBeTrue)
	})

	Convey("ResetChannel 清除该渠道所有模型的冷却条目", t, func() {
		cm := NewCooldownManager(60)
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
		cm := NewCooldownManager(0)
		cm.Put(1, "m1")
		So(cm.IsCoolingDown(1, "m1"), ShouldBeFalse)
	})
}
