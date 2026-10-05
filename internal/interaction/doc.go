// Package interaction 是交互体系的最小实现（架构文档 12）：结构化
// 交互协议（v1 仅 confirm 单类型）的请求-应答-超时-取消闭环。
//
// 协议是领域的，机制是内核的：本包定义交互类型/schema/路由/超时；
// "暂停存点、带值恢复"由内核断点提供（kernel/checkpoint.go）。
// 一致性顺序（崩溃可复盘的根基）：
//  1. 先持久化 checkpoint 再下行交互请求（kernel 在 interrupt 事件上行前落断点，
//     本包在收到中断后才创建请求，顺序天然成立）；
//  2. 先落应答再恢复执行（Reply 先落库再唤醒等待方，等待方随后才 Resume）。
package interaction
