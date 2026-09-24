package domain

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrorClass 决定错误由谁负责，以及能否重试。
type ErrorClass string

const (
	// ClassUser 表示客户端请求本身有问题，换渠道重试也无法成功。
	ClassUser ErrorClass = "user"
	// ClassUpstream 表示上游供应商侧的问题，可换渠道重试。
	ClassUpstream ErrorClass = "upstream"
	// ClassPlatform 表示平台自身故障。
	ClassPlatform ErrorClass = "platform"
)

// Code 是错误码的强类型枚举。字符串取值是对外错误响应体的一部分，不得改动。
//
// 精简到转发场景所需的最小集：保留客户端请求问题、上游故障、平台内部故障与渠道未找到四类。
type Code string

const (
	CodeInvalidRequest      Code = "invalid_request"
	CodeUnauthorized        Code = "unauthorized"
	CodeForbidden           Code = "forbidden"
	CodeRateLimited         Code = "rate_limited"
	CodeGatewayOverloaded   Code = "gateway_overloaded"
	CodeModelNotFound       Code = "model_not_found"
	CodeNotFound            Code = "not_found"
	CodeUpstreamTimeout     Code = "upstream_timeout"
	CodeUpstreamUnavailable Code = "upstream_unavailable"
	CodeUpstreamRateLimited Code = "upstream_rate_limited"
	CodeUpstreamRejected    Code = "upstream_rejected"
	CodeInternal            Code = "internal"
)

// Error 是统一错误类型。
//
// Message 面向用户，Detail 面向排障；两者都不允许携带上游密钥。
type Error struct {
	Code       Code
	Class      ErrorClass
	Retryable  bool
	HTTPStatus int
	Message    string
	Detail     string
	cause      error
}

// Error 实现 error 接口。返回值面向排障，不含用户文案。
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap 返回被包装的底层错误。
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Is 让 errors.Is 按错误码比较，便于调用方只依赖错误码而不依赖具体类型。
func (e *Error) Is(target error) bool {
	var other *Error
	if !errors.As(target, &other) {
		return false
	}
	return e != nil && other != nil && e.Code == other.Code
}

// WithDetail 返回带排障说明的副本，不修改原错误。
func (e *Error) WithDetail(detail string) *Error {
	if e == nil {
		return nil
	}
	clone := *e
	clone.Detail = detail
	return &clone
}

// WithCause 返回带底层错误的副本，不修改原错误。
func (e *Error) WithCause(err error) *Error {
	if e == nil {
		return nil
	}
	clone := *e
	clone.cause = err
	return &clone
}

// NewError 按错误码构造统一错误。错误码决定分级、可重试性与 HTTP 状态码。
//
// message 是面向用户的文案；未登记的 code 一律按平台内部错误处理，
// 避免未分级错误被当成可重试而上游风暴。
func NewError(code Code, message string) *Error {
	class, retryable, status := classify(code)
	return &Error{
		Code:       code,
		Class:      class,
		Retryable:  retryable,
		HTTPStatus: status,
		Message:    message,
	}
}

// classify 返回错误码对应的分级、可重试性与 HTTP 状态码。
func classify(code Code) (ErrorClass, bool, int) {
	switch code {
	case CodeInvalidRequest:
		return ClassUser, false, http.StatusBadRequest
	case CodeUnauthorized:
		return ClassUser, false, http.StatusUnauthorized
	case CodeForbidden:
		// 已通过身份校验但无权访问该资源：换凭证可能成功，但重试同一请求不会，故不可重试。
		return ClassUser, false, http.StatusForbidden
	case CodeRateLimited:
		// 调用方自身超过 RPM/TPM 限额：换成别的渠道也一样超限，故不可重试。
		return ClassUser, false, http.StatusTooManyRequests
	case CodeGatewayOverloaded:
		// 网关自身并发位或等待队列已满：属平台自身故障，换渠道不会腾出网关并发位，
		// 故不可重试；回 429 让客户端退避后重试，避免把已过载的网关继续压垮。
		return ClassPlatform, false, http.StatusTooManyRequests
	case CodeModelNotFound:
		return ClassUser, false, http.StatusNotFound
	case CodeNotFound:
		// 调用者可寻址范围内不存在该资源：换凭证可能改变可见范围，但重试同一请求不会。
		return ClassUser, false, http.StatusNotFound
	case CodeUpstreamTimeout:
		return ClassUpstream, true, http.StatusGatewayTimeout
	case CodeUpstreamUnavailable:
		return ClassUpstream, true, http.StatusBadGateway
	case CodeUpstreamRateLimited:
		// 上游限流：换一个渠道可能成功，故可重试。
		return ClassUpstream, true, http.StatusServiceUnavailable
	case CodeUpstreamRejected:
		return ClassUpstream, false, http.StatusBadGateway
	default:
		return ClassPlatform, false, http.StatusInternalServerError
	}
}

// AsError 从任意 error 中提取统一错误；不是统一错误时返回 nil。
func AsError(err error) *Error {
	var target *Error
	if errors.As(err, &target) {
		return target
	}
	return nil
}

// Retryable 报告该错误是否允许换渠道重试。
func Retryable(err error) bool {
	if e := AsError(err); e != nil {
		return e.Retryable
	}
	return false
}

// HTTPStatus 返回应回给客户端的 HTTP 状态码；非统一错误一律按 500 处理。
func HTTPStatus(err error) int {
	if e := AsError(err); e != nil && e.HTTPStatus != 0 {
		return e.HTTPStatus
	}
	return http.StatusInternalServerError
}
