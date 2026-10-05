// Package configs 向 semantic 安装器提供编译期内置的仓库配置模板。
// 运行时设置绝不能写入这些内置文件；semantic init 会先把它们复制到
// 安装目录，之后 Server、热重载与前端设置只读写安装副本。
package configs

import "embed"

// Templates 包含不可变的 Server 配置模板，以及首次安装所需的默认 Agent
// 和 Skill 目录树。go:embed 让发布后的 CLI 不再依赖源码仓库位置。
//
//go:embed semantic-server.yaml agents skills runtimes.d scenes.d
var Templates embed.FS
