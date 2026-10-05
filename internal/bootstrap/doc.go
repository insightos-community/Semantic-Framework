// Package bootstrap 是 semantic-server 的装配层。
//
// main 只负责解析参数与加载配置，组件的创建、路由注册与生命周期管理
// 全部收敛在本包：wire_access.go 的 Wire 按启动序列装配接入层
// （store 迁移 → auth 种子 → HTTP/WS 网关），bootstrap.go 的 App.Run
// 负责 HTTP/WS 双监听的启动与基于信号的优雅退出（含 store 关闭）。
package bootstrap
