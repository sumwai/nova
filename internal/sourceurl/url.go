// Package sourceurl 归一远端订阅源地址。
//
// 「同一个源」的判定在三处出现：配置层用它做去重，同步层用它当状态键，
// 重定向检查用它比较主机。三处各自实现一定会漂移，而漂移的后果是可被绕过的
// serial 单调性与看起来合法却永久失败的配置，因此归一规则集中在这里。
package sourceurl

import (
	"net"
	"net/url"
	"path"
	"strings"
)

// Normalize 返回一个远端地址的规范形式。
//
// 归一范围限定在「同一地址的不同写法」：scheme 与主机大小写、主机末尾点、
// scheme 默认端口、空路径与结尾斜杠、片段。查询参数原样保留：
// 改名后的查询参数指向另一个资源，归一它会把两个源压成一个。
//
// 解析失败的原文原样返回：归一不是校验，非法地址的处置留给调用方。
func Normalize(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	out := *parsed
	out.Scheme = strings.ToLower(out.Scheme)
	out.Host = CanonicalHost(out.Scheme, out.Host)
	out.Fragment = ""
	if out.Path == "" {
		out.Path = "/"
	}
	out.Path = path.Clean(out.Path)
	return out.String()
}

// CanonicalHost 返回某个 scheme 下 hostport 的规范形式。
//
// 主机名按 DNS 规则大小写不敏感且允许末尾点，端口按 scheme 的默认值归一，
// 因此 https://example.com:443/p 与 https://EXAMPLE.com./p 得到同一个结果。
// 非默认端口原样保留：不同端口是不同来源。
func CanonicalHost(scheme, hostport string) string {
	host, port := hostport, ""
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		host, port = h, p
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if port == defaultPort(scheme) {
		port = ""
	}
	if port == "" {
		// IPv6 字面量带回来时必须补回方括号，否则拼出的 URL 无法再解析。
		if strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	return net.JoinHostPort(host, port)
}

// defaultPort 返回一个 scheme 对应的默认端口；scheme 未知时返回空串。
func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}
