package http

import (
	"encoding/json"
	"net/http"
)

// WriteError 以统一格式 {"error":{"code","message"}} 写出错误响应。
// 全网关（中间件、handler）共用此入口，保证前端只需一种错误解析逻辑。
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": msg},
	})
}

// WriteJSON 以 JSON 格式写出正常响应体。
// 响应头写入失败（连接已断开等）无法重试，编码错误无法回传，直接忽略。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
