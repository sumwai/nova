package gateway

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/transport"
)

// bearerScheme 是 Authorization 头里 Bearer 方案的名字，含它后面的那个空格。
const bearerScheme = "Bearer "

// apiKeyHeader 是客户端可以代替 Authorization 使用的凭据头。
//
// 两种形态都接受，因为调用方的惯例不同：OpenAI 系客户端用 Authorization: Bearer，
// Anthropic 系用 x-api-key。只认一种，会让另一半调用方在「改写请求头」与「改用别的
// 网关」之间做选择，而这两件事都不该由 nova 决定。
const apiKeyHeader = "x-api-key"

// googleAPIKeyHeader 是 Gemini 客户端提交凭据的请求头。
//
// 它的取值是裸密钥（没有 Bearer 方案名），与 x-api-key 同形；单列一个常量是因为
// Gemini 的 SDK 只会发这一个头，不认它就是「Gemini 客户端连不进网关」。
const googleAPIKeyHeader = "x-goog-api-key"

// authorize 按配置里的 client_key 给客户端入口加上客户端鉴权。
//
// keys 为空时原样返回 next：不写 client_key 就是不鉴权。配置层已经为此在「绑到非回环
// 地址」时报过警告，装配层不重复判断——同一件事有两处来源时，两处迟早会不一致。
func authorize(next http.Handler, keys []string, resolve transport.AdapterResolver) http.Handler {
	if len(keys) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := presentedKey(r)
		if !ok || !matchesAnyKey(presented, keys) {
			rejectUnauthorized(w, r, resolve)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// presentedKey 取客户端提交的凭据，第二个返回值报告有没有提交。
//
// 三个头都看，Authorization 优先：它带着方案名，语义比裸密钥头明确。一个同时
// 写了多个头、却只把其中一个写对的客户端，任一种取法都会放行它；差异只出现在
// 「都写且写的是不同 key」这种本就自相矛盾的请求上，此时无论选哪个都不比另一个更正确。
func presentedKey(r *http.Request) (string, bool) {
	if value := r.Header.Get("Authorization"); len(value) > len(bearerScheme) &&
		strings.EqualFold(value[:len(bearerScheme)], bearerScheme) {
		return value[len(bearerScheme):], true
	}
	if value := r.Header.Get(apiKeyHeader); value != "" {
		return value, true
	}
	if value := r.Header.Get(googleAPIKeyHeader); value != "" {
		return value, true
	}
	return "", false
}

// matchesAnyKey 报告提交的凭据是否命中配置里的某一条。
//
// 比较不随内容提前退出：逐字节比对会在第一个不同的字节处返回，于是「前缀猜对了几位」
// 能从响应耗时里读出来。它挡不住长度差异造成的信息泄露，但长度不是秘密——真正的秘密是
// 内容。候选条数是个位数，全量比较的代价可以忽略。
func matchesAnyKey(presented string, keys []string) bool {
	match := false
	for _, key := range keys {
		// 刻意不写成 || 短路：短路会让「第一条就命中」与「最后一条才命中」耗时不同。
		if subtle.ConstantTimeCompare([]byte(presented), []byte(key)) == 1 {
			match = true
		}
	}
	return match
}

// rejectUnauthorized 按客户端协议回一个 401。
//
// 路径受支持时用该路径的适配器编码错误体，让鉴权失败与其它错误的形状一致，调用方因此
// 不必为「先被鉴权挡住」单独写一套解析。路径本身不认识时不泄露路由事实，但仍要说清
// 「缺凭据」：只回 404，会让一个只是漏了 token 的客户端去反复检查自己拼的 URL。
func rejectUnauthorized(w http.ResponseWriter, r *http.Request, resolve transport.AdapterResolver) {
	adapter, ok := resolve(r)
	if !ok {
		http.Error(w, "缺少或无效的客户端凭据", http.StatusUnauthorized)
		return
	}
	err := domain.NewError(domain.CodeUnauthorized, "缺少或无效的客户端凭据")
	status, body := adapter.EncodeError(err)
	w.Header().Set("Content-Type", adapter.ContentType())
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
