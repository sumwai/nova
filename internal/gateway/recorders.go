package gateway

import (
	"context"

	"github.com/sumwai/nova/internal/domain"
	"github.com/sumwai/nova/internal/stats"
	"github.com/sumwai/nova/internal/transport"
)

// recorders 把一次观测同时交给日志与统计。
//
// 两者消费同一对接口，因此在装配层做一次扇出即可，转发路径完全不必知道有几个消费者。
// 不把统计写进 logger：日志句柄随装配整体换入换出，而统计必须跨 reload 连续，
// 两者的生命周期不同，混在一个对象里会让「换日志级别」顺带清空统计。
type recorders struct {
	log   *logger
	stats *stats.Store
}

// 编译期断言：扇出对象要同时满足入口层与流水线两边的观测接口。
var (
	_ transport.AccessLogger = (*recorders)(nil)
	_ domain.Observer        = (*recorders)(nil)
)

func (r *recorders) LogAccess(rec transport.AccessRecord) {
	r.log.LogAccess(rec)
	if r.stats != nil {
		r.stats.LogAccess(rec)
	}
}

// RecordAttempt 把尝试记录交给日志与统计，并返回日志侧的结果。
//
// 统计侧的返回值恒为 nil，因此只有日志的失败可能被抛回流水线；这个错误在
// logger 那边也恒为 nil，保留透传是为了不在装配层替它决定语义。
func (r *recorders) RecordAttempt(ctx context.Context, rec domain.AttemptRecord) error {
	err := r.log.RecordAttempt(ctx, rec)
	if r.stats != nil {
		_ = r.stats.RecordAttempt(ctx, rec)
	}
	return err
}
