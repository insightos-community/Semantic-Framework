package version

// Version 是框架的语义版本号，由 Makefile 中 -ldflags 在编译时注入。
// 直接以源码运行（go run）时使用当前开发版本，正式构建仍由 Makefile 注入。
var Version = "0.5.0-dev"

// String 返回当前版本号字符串，供 HTTP 接口与 CLI 输出使用。
func String() string {
	return Version
}
