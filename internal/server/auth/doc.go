// Package auth 实现 semantic-server 的本地账号认证。
//
// 首版仅实现本地账号体系（架构文档 §3.6：零外部依赖可启动）：
// 用户名/密码登录（bcrypt 校验）签发 24h 访问令牌，HTTP 中间件按
// Bearer token 鉴权并注入 user_id。对外接口只有 Service 与 Handlers，
// 错误统一为带 code 的 Error 类型，HTTP 层直接透传 code 给前端。
package auth
