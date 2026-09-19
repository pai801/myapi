package web

import "embed"

// BuildFS 内嵌编译好的前端产物（web/build）。
//
// 单独放进可 import 的包，是因为 //go:embed 不能跨包、不支持 "../"、不支持符号链接：
// 外部扩展方的 main（以及本仓的 app.Run）都需要通过 import 本包取得同一份前端 FS，
// 而无法各自 embed 到本仓根目录下的 web/build。
//
//go:embed build/*
var BuildFS embed.FS
