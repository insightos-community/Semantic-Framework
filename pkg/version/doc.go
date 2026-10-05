// Package version 提供框架统一的版本号信息。
//
// 版本号通过 Makefile 的 -ldflags 在编译时注入，未注入时使用开发默认值，
// 确保 server、CLI 与 HTTP 接口暴露的版本标识始终一致。
package version
