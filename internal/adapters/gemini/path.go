package gemini

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/sumwai/nova/internal/domain"
)

// modelPlaceholder 是上游端点地址里指代模型名的占位符。
//
// 模型名不在请求体里，只能靠地址模板与选路结果在发送前合到一起。配置层要求
// Gemini 端点的 url 必须含它（见 internal/config 的同名常量）。
const modelPlaceholder = "{model}"

// modelPrefix 是 Google 的资源名前缀，客户端与上游都用 `models/<id>` 寻址。
//
// 选路表里的对外名不带这个前缀（`model gemini-2.5-flash`），因此解析客户端路径时
// 把它去掉；需要带前缀的上游由地址模板里的 `/models/` 段负责。
const modelPrefix = "models/"

// MatchRequestPath 判定一条客户端路径是不是 Gemini 的 generateContent 端点。
//
// 命中时返回路径里的模型名与是否流式。识别规则：
//   - 末段是以 `:` 开头的动作，且动作是 generateContent 或 streamGenerateContent；
//   - 动作之前是 `<版本根>/<资源名>`，版本根形如 v1beta / v1，资源名前缀 models/ 去掉。
//
// 之所以不用等值比较：这条路径里带着模型名，不是定长字面量，这也是 Gemini 与另外三种
// 协议在入口层唯一的差别。
func MatchRequestPath(path string) (model string, stream bool, ok bool) {
	colon := strings.LastIndex(path, ":")
	if colon < 0 {
		return "", false, false
	}
	switch path[colon+1:] {
	case strings.TrimPrefix(actionGenerate, ":"):
		stream = false
	case strings.TrimPrefix(actionStreamGenerate, ":"):
		stream = true
	default:
		return "", false, false
	}

	segments := strings.Split(strings.Trim(path[:colon], "/"), "/")
	if len(segments) < 2 || !isVersionSegment(segments[0]) {
		return "", false, false
	}
	resource := strings.Join(segments[1:], "/")
	if !strings.HasPrefix(resource, modelPrefix) {
		return "", false, false
	}
	model = strings.TrimPrefix(resource, modelPrefix)
	if model == "" || strings.Contains(model, ":") {
		return "", false, false
	}
	return model, stream, true
}

// isVersionSegment 报告一个路径段是否是 Google 的版本根（v1、v1beta、v1alpha1 等）。
//
// 只按形状判定，不列举具体取值：上游新增版本号不应该要求网关同步改一份清单。
func isVersionSegment(segment string) bool {
	if len(segment) < 2 || segment[0] != 'v' || segment[1] < '0' || segment[1] > '9' {
		return false
	}
	for i := 1; i < len(segment); i++ {
		c := segment[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		default:
			return false
		}
	}
	return true
}

// BindRequest 把请求路径上的模型名与流式标记绑定到一个只服务本次请求的副本上。
//
// 查询参数 alt=sse 也算流式：`...:generateContent?alt=sse` 与
// `...:streamGenerateContent` 是两种等价的流式写法。
func (a *Adapter) BindRequest(path string, query url.Values) domain.Adapter {
	bound := &Adapter{}
	model, stream, ok := MatchRequestPath(path)
	if !ok {
		bound.bindErr = invalidRequest("请求路径不是 Gemini 的 generateContent 端点")
		return bound
	}
	if query != nil && query.Get("alt") == "sse" {
		stream = true
	}
	bound.boundModel = model
	bound.boundStream = stream
	return bound
}

// UpstreamURL 按本次调用构造上游地址。
//
// 端点在配置里写成 `.../models/{model}:generateContent`，这里做两件事：
//   - 把占位符换成 Route.UpstreamModel；
//   - 按本次是否流式把动作段改成 generateContent 或 streamGenerateContent，
//     并相应设置 alt=sse。
//
// 地址模板由配置写全，本方法只在两处已知位置上改写，因此不会把一个写错版本的地址
// 悄悄纠正成另一个：末段不是动作段时报错，由使用者回去改配置。
func (a *Adapter) UpstreamURL(route domain.Route, stream bool) (string, error) {
	template := strings.TrimSpace(route.BaseURL)
	if template == "" {
		return "", fmt.Errorf("端点地址为空")
	}
	model := strings.TrimSpace(route.UpstreamModel)
	if model == "" {
		return "", fmt.Errorf("端点 %q 缺少上游模型名", route.BaseURL)
	}
	if !strings.Contains(template, modelPlaceholder) {
		return "", fmt.Errorf("端点地址 %q 缺少 %s 占位符", route.BaseURL, modelPlaceholder)
	}

	parsed, err := url.Parse(strings.ReplaceAll(template, modelPlaceholder, model))
	if err != nil {
		return "", fmt.Errorf("解析端点地址失败：%w", err)
	}
	switch {
	case strings.HasSuffix(parsed.Path, actionStreamGenerate):
		if !stream {
			parsed.Path = strings.TrimSuffix(parsed.Path, actionStreamGenerate) + actionGenerate
		}
	case strings.HasSuffix(parsed.Path, actionGenerate):
		if stream {
			parsed.Path = strings.TrimSuffix(parsed.Path, actionGenerate) + actionStreamGenerate
		}
	default:
		return "", fmt.Errorf("端点地址 %q 的末段不是 %s 或 %s",
			route.BaseURL, actionGenerate, actionStreamGenerate)
	}
	// 让 url 包按新的 Path 重新计算转义原文，避免保留旧路径的 RawPath。
	parsed.RawPath = ""
	query := parsed.Query()
	if stream {
		query.Set("alt", "sse")
	} else {
		query.Del("alt")
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
