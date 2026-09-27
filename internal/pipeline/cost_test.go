package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/domain"
)

// costStub 是一个固定返回的成本估算器。
type costStub struct {
	cost domain.Cost
	ok   bool
}

func (c costStub) Cost(domain.Route, domain.Usage) (domain.Cost, bool) { return c.cost, c.ok }

// attemptCollector 收集写出的尝试记录。
type attemptCollector struct {
	records []domain.AttemptRecord
}

func (c *attemptCollector) RecordAttempt(_ context.Context, rec domain.AttemptRecord) error {
	c.records = append(c.records, rec)
	return nil
}

// TestRecordAttemptAttachesCost 守护成本估算结果被写进尝试记录。
func TestRecordAttemptAttachesCost(t *testing.T) {
	start := time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)
	usage := domain.Usage{Source: domain.UsageSourceUpstream, InputTokens: 10, OutputTokens: 20}

	collector := &attemptCollector{}
	pipe := &Pipeline{
		observer: collector,
		costs:    costStub{cost: domain.Cost{Currency: "USD", Amount: 1.5, Estimated: true}, ok: true},
	}
	pipe.recordAttempt(context.Background(), &domain.Request{RequestID: "r1"}, domain.Route{},
		1, attemptResult{Usage: usage, StartedAt: start, EndedAt: start.Add(time.Millisecond)})

	if len(collector.records) != 1 {
		t.Fatalf("记录数 = %d，期望 1", len(collector.records))
	}
	cost := collector.records[0].Cost
	if cost == nil || cost.Currency != "USD" || cost.Amount != 1.5 || !cost.Estimated {
		t.Fatalf("成本 = %+v，期望 USD 1.5（估算）", cost)
	}
}

// TestRecordAttemptWithoutCostLeavesItNil 守护「没有价格」不写成零成本。
func TestRecordAttemptWithoutCostLeavesItNil(t *testing.T) {
	start := time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)
	collector := &attemptCollector{}
	pipe := &Pipeline{observer: collector, costs: costStub{ok: false}}

	pipe.recordAttempt(context.Background(), &domain.Request{RequestID: "r1"}, domain.Route{},
		1, attemptResult{StartedAt: start, EndedAt: start})

	if len(collector.records) != 1 || collector.records[0].Cost != nil {
		t.Fatalf("没有价格时成本应为 nil，实际 %+v", collector.records)
	}
}
