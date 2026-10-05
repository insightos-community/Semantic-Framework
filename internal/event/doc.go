// Package event 提供进程内事件总线。
//
// 总线连接事件的发布者（Agent 运行时、任务体系等域模块）与订阅者
// （WS 网关等接入层组件）：发布方只面向 topic 投递，不关心谁订阅；
// 订阅方按 topic 接收 Event。Publish 异步非阻塞——订阅者消费不及时
// 时丢弃事件并记录 DEBUG 日志，保证慢订阅者不会拖垮发布链路。
package event
