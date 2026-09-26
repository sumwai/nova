package pipeline

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/sumwai/nova/internal/adapters/openaichat"
	"github.com/sumwai/nova/internal/domain"
)

// testRequest 造一个可定稿的 OpenAI Chat 请求。
func testRequest() *domain.Request {
	return &domain.Request{
		RequestID: "req-1",
		Protocol:  domain.ProtocolOpenAIChat,
		Model:     "m",
		Messages: []domain.Message{{
			Role:  domain.RoleUser,
			Parts: []domain.Part{{Kind: domain.PartText, Text: "hi"}},
		}},
		RawBody: []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
	}
}

// testRoute 造一条 OpenAI Chat 上游路由。
func testRoute() domain.Route {
	return domain.Route{
		UpstreamID:    "relay host",
		Provider:      "relay",
		Protocol:      domain.ProtocolOpenAIChat,
		UpstreamModel: "m",
		BaseURL:       "https://relay.example.com/v1/chat/completions",
	}
}

// fakeResolver 返回固定候选或固定错误，并记录调用次数。
type fakeResolver struct {
	routes []domain.Route
	err    error
	calls  int
}

func (f *fakeResolver) Candidates(context.Context, *domain.Request) ([]domain.Route, error) {
	f.calls++
	return f.routes, f.err
}

// fakeUpstream 按调用序号返回结果或错误，最后一个之后复用最后一个。
type fakeUpstream struct {
	results []*domain.UpstreamResult
	errs    []error
	panics  bool
	calls   int
}

func (f *fakeUpstream) Complete(context.Context, domain.Route, *domain.Request, []byte) (*domain.UpstreamResult, error) {
	index := f.calls
	f.calls++
	if f.panics {
		panic("上游调用 panic")
	}
	if index < len(f.errs) && f.errs[index] != nil {
		return nil, f.errs[index]
	}
	if index < len(f.results) {
		return f.results[index], nil
	}
	return &domain.UpstreamResult{Raw: []byte(`{"ok":true}`), Response: &domain.Response{}}, nil
}

func (f *fakeUpstream) Stream(context.Context, domain.Route, *domain.Request, []byte, domain.ChunkSink) error {
	return nil
}

// settleRecord 是一次预留结算的现场。
type settleRecord struct {
	usage domain.Usage
	err   error
}

// observeRecord 是一次响应头观测的现场。
type observeRecord struct {
	header    http.Header
	succeeded bool
}

// fakeLimits 记录预留、结算、观测与错误学习，供断言调用路径。
type fakeLimits struct {
	reject  bool
	est     CostEstimate
	hasEst  bool
	settles []settleRecord
	observe []observeRecord
	learn   []error
}

func (f *fakeLimits) Reserve(_ domain.Route, _ string, est CostEstimate) (func(domain.Usage, error), bool) {
	f.est = est
	f.hasEst = true
	if f.reject {
		return nil, false
	}
	return func(usage domain.Usage, err error) {
		f.settles = append(f.settles, settleRecord{usage: usage, err: err})
	}, true
}

func (f *fakeLimits) Observe(_ domain.Route, _ string, header http.Header, succeeded bool) {
	f.observe = append(f.observe, observeRecord{header: header, succeeded: succeeded})
}

func (f *fakeLimits) LearnExhausted(_ domain.Route, _ string, err error) {
	f.learn = append(f.learn, err)
}

// newTestPipeline 用固定候选与固定上游组装一条流水线。
func newTestPipeline(t *testing.T, resolver domain.RouteResolver, upstream domain.UpstreamCaller, limits LimitRuntime) *Pipeline {
	t.Helper()
	pipeline, err := New(Options{
		Adapters: func(domain.Protocol) (domain.Adapter, error) { return openaichat.New(), nil },
		Upstream: upstream,
		Routes:   resolver,
		Limits:   limits,
	})
	if err != nil {
		t.Fatalf("构造流水线失败：%v", err)
	}
	return pipeline
}

// TestForwardSettlesReservationOnSuccess 守护成功尝试按实际用量结算预留。
func TestForwardSettlesReservationOnSuccess(t *testing.T) {
	usage := domain.Usage{Source: domain.UsageSourceUpstream, InputTokens: 3, OutputTokens: 4}
	upstream := &fakeUpstream{results: []*domain.UpstreamResult{{
		Raw:      []byte(`{"ok":true}`),
		Response: &domain.Response{Usage: usage},
		Headers:  http.Header{"X-Ratelimit-Remaining-Requests": []string{"5"}},
	}}}
	limits := &fakeLimits{}
	pipeline := newTestPipeline(t, &fakeResolver{routes: []domain.Route{testRoute()}}, upstream, limits)

	if err := pipeline.Forward(context.Background(), openaichat.New(), testRequest(), &bytes.Buffer{}); err != nil {
		t.Fatalf("转发应成功：%v", err)
	}
	if len(limits.settles) != 1 {
		t.Fatalf("预留应结算一次，实际 %d 次", len(limits.settles))
	}
	if !limits.hasEst || limits.est.Requests != 1 {
		t.Fatalf("预留估量应带 requests=1，实际 %+v", limits.est)
	}
	if limits.est.Tokens < 1 {
		t.Fatalf("预留估量 tokens 应至少为 1，实际 %v", limits.est.Tokens)
	}
	if limits.settles[0].err != nil {
		t.Fatalf("成功尝试的结算错误应为 nil，实际 %v", limits.settles[0].err)
	}
	if got := limits.settles[0].usage.Total(); got != 7 {
		t.Fatalf("结算用量 total=%d，期望 7", got)
	}
	if len(limits.observe) != 1 || !limits.observe[0].succeeded {
		t.Fatalf("成功尝试应观测一次且 succeeded=true，实际 %+v", limits.observe)
	}
	if len(limits.learn) != 0 {
		t.Fatalf("成功尝试不应触发错误学习，实际 %d 次", len(limits.learn))
	}
}

// TestForwardSettlesReservationOnFailure 守护失败尝试回滚预留，并学习额度耗尽。
func TestForwardSettlesReservationOnFailure(t *testing.T) {
	quotaErr := domain.NewError(domain.CodeUpstreamQuotaExhausted, "上游额度耗尽")
	upstream := &fakeUpstream{errs: []error{quotaErr}}
	limits := &fakeLimits{}
	pipeline := newTestPipeline(t, &fakeResolver{routes: []domain.Route{testRoute()}}, upstream, limits)

	err := pipeline.Forward(context.Background(), openaichat.New(), testRequest(), &bytes.Buffer{})
	if domainErr := domain.AsError(err); domainErr == nil || domainErr.Code != domain.CodeUpstreamQuotaExhausted {
		t.Fatalf("应原样返回上游额度耗尽错误，实际 %v", err)
	}
	if len(limits.settles) != 1 || limits.settles[0].err == nil {
		t.Fatalf("失败尝试应以错误结算一次，实际 %+v", limits.settles)
	}
	if len(limits.observe) != 1 || limits.observe[0].succeeded {
		t.Fatalf("失败尝试应观测一次且 succeeded=false，实际 %+v", limits.observe)
	}
	if len(limits.learn) != 1 {
		t.Fatalf("额度耗尽失败应交给错误学习一次，实际 %d 次", len(limits.learn))
	}
}

// TestForwardAllCandidatesQuotaRejected 守护「所有候选被额度剔除」回额度耗尽码而不是 404。
func TestForwardAllCandidatesQuotaRejected(t *testing.T) {
	upstream := &fakeUpstream{}
	limits := &fakeLimits{reject: true}
	pipeline := newTestPipeline(t, &fakeResolver{routes: []domain.Route{testRoute()}}, upstream, limits)

	err := pipeline.Forward(context.Background(), openaichat.New(), testRequest(), &bytes.Buffer{})
	domainErr := domain.AsError(err)
	if domainErr == nil || domainErr.Code != domain.CodeUpstreamQuotaExhausted {
		t.Fatalf("全部候选被额度剔除应报额度耗尽，实际 %v", err)
	}
	if upstream.calls != 0 {
		t.Fatalf("没有候选可用时不得发起上游调用，实际 %d 次", upstream.calls)
	}
}

// TestForwardPropagatesCandidateDomainError 守护选路层用统一错误表达语义时被原样透传。
func TestForwardPropagatesCandidateDomainError(t *testing.T) {
	quotaErr := domain.NewError(domain.CodeUpstreamQuotaExhausted, "所有候选渠道的额度均不可用")
	resolver := &fakeResolver{err: quotaErr}
	pipeline := newTestPipeline(t, resolver, &fakeUpstream{}, nil)

	err := pipeline.Forward(context.Background(), openaichat.New(), testRequest(), &bytes.Buffer{})
	domainErr := domain.AsError(err)
	if domainErr == nil || domainErr.Code != domain.CodeUpstreamQuotaExhausted {
		t.Fatalf("选路层的统一错误应原样返回，实际 %v", err)
	}
	if domainErr.Code == domain.CodeInternal {
		t.Fatal("选路层的语义错误不得被改判为内部故障")
	}
}

// TestForwardWrapsUnclassifiedCandidateError 守护非统一错误仍归为选路失败。
func TestForwardWrapsUnclassifiedCandidateError(t *testing.T) {
	resolver := &fakeResolver{err: errors.New("boom")}
	pipeline := newTestPipeline(t, resolver, &fakeUpstream{}, nil)

	err := pipeline.Forward(context.Background(), openaichat.New(), testRequest(), &bytes.Buffer{})
	domainErr := domain.AsError(err)
	if domainErr == nil || domainErr.Code != domain.CodeInternal {
		t.Fatalf("非统一错误应归为选路失败，实际 %v", err)
	}
	if !errors.Is(err, resolver.err) {
		t.Fatal("原始错误应作为 cause 保留")
	}
}

// TestForwardSettlesReservationOnFinalizeFailure 守护请求定稿失败时预留被回滚，
// 且不发起上游调用。
func TestForwardSettlesReservationOnFinalizeFailure(t *testing.T) {
	limits := &fakeLimits{}
	upstream := &fakeUpstream{}
	pipeline, err := New(Options{
		Adapters: func(domain.Protocol) (domain.Adapter, error) { return nil, errors.New("没有适配器") },
		Upstream: upstream,
		Routes:   &fakeResolver{routes: []domain.Route{testRoute()}},
		Limits:   limits,
	})
	if err != nil {
		t.Fatalf("构造流水线失败：%v", err)
	}

	err = pipeline.Forward(context.Background(), openaichat.New(), testRequest(), &bytes.Buffer{})
	if domainErr := domain.AsError(err); domainErr == nil || domainErr.Code != domain.CodeInternal {
		t.Fatalf("定稿失败应报内部错误，实际 %v", err)
	}
	if upstream.calls != 0 {
		t.Fatalf("定稿失败时不得发起上游调用，实际 %d 次", upstream.calls)
	}
	if len(limits.settles) != 1 || limits.settles[0].err == nil {
		t.Fatalf("定稿失败应以错误结算预留一次，实际 %+v", limits.settles)
	}
}

// TestForwardSettlesReservationOnPanic 守护 doAttempt panic 时预留仍被回滚。
func TestForwardSettlesReservationOnPanic(t *testing.T) {
	limits := &fakeLimits{}
	pipeline := newTestPipeline(t, &fakeResolver{routes: []domain.Route{testRoute()}}, &fakeUpstream{panics: true}, limits)

	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("doAttempt panic 应继续向上传播")
		}
		if len(limits.settles) != 1 {
			t.Fatalf("panic 时预留应被结算一次，实际 %d 次", len(limits.settles))
		}
		if limits.settles[0].err == nil {
			t.Fatal("panic 路径的结算必须带错误")
		}
	}()
	_ = pipeline.Forward(context.Background(), openaichat.New(), testRequest(), &bytes.Buffer{})
}
