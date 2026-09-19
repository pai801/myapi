package relay

import (
	"github.com/pai801/myapi/relay/adaptor"
)

// GetAdaptor 返回 apiType 对应的 adaptor，未登记时返回 nil。
// 各内置渠道通过 relay/register.go 的 adaptor.Register 集中登记（PRD §5.1 / §5.2）。
func GetAdaptor(apiType int) adaptor.Adaptor {
	return adaptor.Get(apiType)
}
