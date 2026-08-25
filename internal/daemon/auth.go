package daemon

import (
	"net/http"
	"strings"
)

// AuthMiddleware 为 daemon HTTP 端点提供 Bearer token 校验。
// 返回一个包装中间件：校验通过则放行，否则 401。
//
// 当前实现：本次面向本地桌面端、仅绑定 127.0.0.1，token 校验为占位——
// 未配置 token 时默认放行。若后续需跨 profile / 远程暴露，应在此注入
// token + scope 的校验逻辑。
//
// TODO(module2): 按 profile 注入 token+scope，支持多实例隔离与权限收敛。
func AuthMiddleware(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		// 未配置 token：放行（仅回环时建议继续启用绑定限制）。
		if token == "" {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 从 Authorization 头提取 Bearer token。
			header := r.Header.Get("Authorization")
			parts := strings.SplitN(header, " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && parts[1] == token {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
	}
}
