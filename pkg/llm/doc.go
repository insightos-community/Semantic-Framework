// Package llm 提供 LLM 提供方注册表与计量类型。
//
// 本包只建模配置与注册表，不 import 任何内核依赖（eino/eino-ext）：
// 内核适配收敛在 internal/agent/kernel（架构文档 01 §3 的 ACL 纪律）。
// api_key 不进入配置结构，解析顺序为：进程环境变量
// SEMANTIC_LLM_API_KEY_<名称大写>（含 .env 注入，同一 base_url 的多个
// 条目共享同一把 key，docs/architecture/02 §5）> 服务端托管密钥库
// （KeyStore 接口，internal/store 实现，bootstrap 装配时注入，nil 时仅
// env）。解析结果按端点名缓存（含来源标记），Reload/InvalidateKeyCache
// 清空重读。
package llm
