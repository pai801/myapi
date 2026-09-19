package relay

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"

	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/apitype"
)

func TestGetAdaptor(t *testing.T) {
	Convey("get adaptor", t, func() {
		// 每个已注册的 apiType 都必须能取到非 nil adaptor。
		// 遍历 registry 而非硬编码区间：将来新增/摘除渠道后自动适配。
		for _, apiType := range adaptor.RegisteredAPITypes() {
			a := GetAdaptor(apiType)
			So(a, ShouldNotBeNil)
		}
		Convey("未注册的 apiType 返回 nil", func() {
			// 哨兵值：明显不在任何枚举区间内，等价于"未注册的扩展渠道"
			So(GetAdaptor(apitype.Dummy+1), ShouldBeNil)
		})
	})
}
