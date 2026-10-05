// semantic CLI：init / login / chat / sessions / doctor 子命令。
// 标准库 flag 分发（不引入 cobra），REST 走标准库，WS 走 coder/websocket。
package main

import (
	"flag"
	"fmt"
	"os"

	"insightos.cn/semantic-framework/pkg/config"
)

// defaultServer 是未指定 --server 且本地无登录凭据时的缺省服务地址。
const defaultServer = "http://127.0.0.1:8080"

// bootDotEnv 是 main 早期加载 .env 的结果（doctor 的 .env 检查直接复用，
// 避免二次加载把"已设置的键"误报为 0）。
var bootDotEnv struct {
	result config.DotEnvResult
	err    error
}

func main() {
	// .env 解析失败不阻塞 CLI：登录/对话不依赖它；doctor 会把它列为 ✗。
	bootDotEnv.result, bootDotEnv.err = config.LoadDotEnv()
	if bootDotEnv.err != nil {
		fmt.Fprintf(os.Stderr, "警告: .env 加载失败: %v\n", bootDotEnv.err)
	}

	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "build":
		os.Exit(runBuild(os.Args[2:]))
	case "init":
		os.Exit(runInit(os.Args[2:]))
	case "install":
		os.Exit(runInstall(os.Args[2:]))
	case "uninstall":
		os.Exit(runUninstall(os.Args[2:]))
	case "login":
		os.Exit(runLogin(os.Args[2:]))
	case "chat":
		os.Exit(runChat(os.Args[2:]))
	case "sessions":
		os.Exit(runSessions(os.Args[2:]))
	case "runtime":
		os.Exit(runRuntime(os.Args[2:]))
	case "doctor":
		os.Exit(runDoctor(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n", os.Args[1])
		usage()
	}
}

// usage 打印用法并以状态码 2 退出（用法错误的约定退出码）。
func usage() {
	fmt.Fprintln(os.Stderr, `semantic — Semantic Framework 管理 CLI

用法:
  semantic install <包|源码目录> --project <项目ID>                安装组件；--robot 绑定设备，--apply 空闲生效
  semantic build <源码目录> --output <安装包.zip>                  构建组件或安装包，支持 --wheel-dir 和 --offline
  semantic install runtime <参数>                                  安装 Runtime Pack（复用 runtime install）
  semantic init [-c 配置文件路径] [--force] [--reset-data]        安装配置；可备份并重建开发数据
  semantic login --username <名> --password <密> [--server 地址]   登录并保存凭据（~/.semantic/credentials.json）
  semantic chat [--session 会话ID] [--server 地址] [--ws 地址]     进入对话 REPL（缺省新建会话，/quit 退出）
  semantic sessions [--server 地址]                                列出我的会话
  semantic doctor [-c 配置文件路径]                                 启动前环境自检
  semantic runtime <install|doctor|upgrade|uninstall|test-start>     安装和管理 Runtime Pack
  semantic runtime <list|check|register> [参数]                     查看或登记已有 Runtime`)
	os.Exit(2)
}

// serverFlag 注册 chat/sessions 共用的 --server flag：空表示"取登录凭据
// 保存的地址，凭据缺省再回落 defaultServer"（login 的 --server 语义不同——
// 显式指定本次登录的目标，缺省 defaultServer）。
func serverFlag(fs *flag.FlagSet) *string {
	return fs.String("server", "", "服务地址（默认取登录凭据中的地址，缺省 "+defaultServer+"）")
}

// resolveServer 按"flag > 凭据 > 缺省"的优先级解析服务地址。
func resolveServer(flagValue string, creds credentials) string {
	if flagValue != "" {
		return flagValue
	}
	if creds.Server != "" {
		return creds.Server
	}
	return defaultServer
}
