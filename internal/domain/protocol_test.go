package domain

import (
	"strings"
	"testing"
)

// TestProtocolEndpointPath 守护端点路径的唯一来源。
//
// 这三个字面量决定「客户端从哪条路径进来」，装配层按它把路径映射为协议；
// 一旦改动或漏项，请求会落到未注册路径上并回 404，因此在此锁死取值。
func TestProtocolEndpointPath(t *testing.T) {
	cases := map[Protocol]string{
		ProtocolOpenAIChat:        "/v1/chat/completions",
		ProtocolOpenAIResponses:   "/v1/responses",
		ProtocolAnthropicMessages: "/v1/messages",
		// 未登记协议没有端点：调用方据此判定该协议不可路由。
		Protocol("unknown_protocol"): "",
	}
	for protocol, want := range cases {
		if got := protocol.EndpointPath(); got != want {
			t.Errorf("%q.EndpointPath() = %q，期望 %q", string(protocol), got, want)
		}
	}
}

// TestProtocolEndpointSegmentMatchesEndpointPath 守护端点段与端点路径同源。
//
// 客户端端点路径是「版本根 /v1 + 端点段」，而上游 url 只比对端点段
// （各供应商把版本写在路径的哪一段并不一致）。两条事实分别写在一处 switch 里，
// 只改其中一处会让比对与对外端点悄悄分叉：装配期据此校验配置，分叉后会误拒合法地址
// 或放过漏写端点路径的地址。这里用拼接关系把两者钉在一起，改一处漏改另一处即失败。
func TestProtocolEndpointSegmentMatchesEndpointPath(t *testing.T) {
	for _, protocol := range []Protocol{
		ProtocolOpenAIChat,
		ProtocolOpenAIResponses,
		ProtocolAnthropicMessages,
	} {
		want := "/v1" + protocol.EndpointSegment()
		if got := protocol.EndpointPath(); got != want {
			t.Errorf("%q.EndpointPath() = %q，期望 \"/v1\" 与端点段 %q 的拼接 %q",
				string(protocol), got, protocol.EndpointSegment(), want)
		}
	}
}

// TestProtocolForEndpointPath 守护「由上游地址的路径推导协议」的判据。
//
// 上游地址只要求以协议的端点段结尾（版本根由各供应商自定），因此推导只看路径后缀：
// 配置层据此校验 url 写没写对端点，以及在省略 protocol 时确定协议。
// 推导不出来与「命中了多个端点段」都必须返回 false：调用方据此报「地址写错了」，
// 而不是随手挑一个协议，把请求按另一种线格式发出去。
func TestProtocolForEndpointPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		want Protocol
		ok   bool
	}{
		{
			name: "openai_chat 的端点段",
			path: "/v1/chat/completions",
			want: ProtocolOpenAIChat, ok: true,
		},
		{
			name: "openai_responses 的端点段",
			path: "/v1/responses",
			want: ProtocolOpenAIResponses, ok: true,
		},
		{
			name: "anthropic_messages 的端点段",
			path: "/v1/messages",
			want: ProtocolAnthropicMessages, ok: true,
		},
		{
			// 版本根写在路径的哪一段由供应商自定，智谱的 OpenAI 兼容端点就是 /api/coding/paas/v4。
			name: "版本根与 /v1 不同",
			path: "/api/coding/paas/v4/chat/completions",
			want: ProtocolOpenAIChat, ok: true,
		},
		{name: "空路径推导不出协议", path: "", ok: false},
		{name: "只有版本根推导不出协议", path: "/v1", ok: false},
		{name: "列表端点不是会话端点", path: "/v1/models", ok: false},
		{name: "端点段出现在中间不算结尾", path: "/v1/chat/completions/extra", ok: false},
		{name: "端点段带尾斜杠不算命中", path: "/v1/messages/", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ProtocolForEndpointPath(tc.path)
			if ok != tc.ok {
				t.Fatalf("ProtocolForEndpointPath(%q) 是否命中应为 %v，实际 %v（协议 %q）",
					tc.path, tc.ok, ok, string(got))
			}
			if got != tc.want {
				t.Errorf("ProtocolForEndpointPath(%q) = %q，期望 %q", tc.path, string(got), string(tc.want))
			}
		})
	}
}

// TestProtocolEndpointSegmentsArePairwiseNotSuffix 守护「端点段两两不互为后缀」这一前提。
//
// 它就是 ProtocolForEndpointPath 唯一命中的成立依据：判据是「路径是否以某段结尾」，
// 一旦某段是另一段的后缀（例如给新协议取 /completions，而既有段是 /chat/completions），
// 同一个地址就会同时以两段结尾，「命中多个即返回 false」那条分支从不可达变成必然可达——
// 两个协议都说得通的配置被静默判为无法推导，或者反过来被解释成其中某一个。
//
// 当前三个合法端点段两两不互为后缀，那条分支因此实际不可达；本用例把「不可达」这件事钉住。
// 下面的清单与 protocol.go 里 EndpointSegment 的 switch 是同一份事实的两处书写：
// 新增协议时要把它一并加进来，若新段与既有段重叠，这里立刻失败，
// 而不是让歧义留到运行期由某个地址的写法去触发。
func TestProtocolEndpointSegmentsArePairwiseNotSuffix(t *testing.T) {
	protocols := []Protocol{
		ProtocolOpenAIChat,
		ProtocolOpenAIResponses,
		ProtocolAnthropicMessages,
	}
	for _, outer := range protocols {
		for _, inner := range protocols {
			if outer == inner {
				continue
			}
			segment, other := outer.EndpointSegment(), inner.EndpointSegment()
			// 空的端点段对任何路径都是后缀，它是「协议没登记」的形态，同样不该在这里出现。
			if segment == "" {
				t.Errorf("%q 没有端点段，无法参与唯一命中的推导", string(outer))
				continue
			}
			if strings.HasSuffix(segment, other) {
				t.Errorf("端点段 %q 以 %q 结尾：%q 的地址会同时命中两个协议，唯一的协议推导不再成立",
					segment, other, string(outer))
			}
		}
	}
}

// joinedUpstreamIDs 把候选列表压成「、」连接的标识串，供本文件按顺序整体比对分区结果。
func joinedUpstreamIDs(routes []Route) string {
	ids := make([]string, 0, len(routes))
	for _, route := range routes {
		ids = append(ids, route.UpstreamID)
	}
	return strings.Join(ids, "、")
}

// TestPreferRoutesForProtocolKeepsEveryCandidate 守护「只调顺序、一条候选都不丢」的稳定划分。
//
// 客户端进来的协议决定哪条候选先上：同协议的候选排到最前，其余候选按原顺序接在后面当回退。
// 这里必须一条都不丢——若像 FilterRoutesForProtocol 那样按协议过滤，客户端发 anthropic_messages
// 而配置里只有 openai_chat 端点时就一条候选都不剩，跨协议重建这条路被关掉。
// 两组各自保持声明顺序：回退链路里谁先谁后是配置写下的，本函数只能改「谁在最前」，
// 不得把链内的相对顺序搅乱，否则同协议候选之间的声明顺序会悄悄失效。
// 同时守住「不改入参、不与入参共享底层数组」：调用方持有返回值逐条推进候选，
// 共享内存会让它的推进意外改写选路表。
func TestPreferRoutesForProtocolKeepsEveryCandidate(t *testing.T) {
	route := func(id string, protocol Protocol) Route {
		return Route{UpstreamID: id, Protocol: protocol}
	}
	cases := []struct {
		name     string
		protocol Protocol
		routes   []Route
		want     []string
	}{
		{
			name:     "同协议候选排在跨协议候选之后",
			protocol: ProtocolOpenAIChat,
			routes: []Route{
				route("anthropic", ProtocolAnthropicMessages),
				route("chat", ProtocolOpenAIChat),
			},
			want: []string{"chat", "anthropic"},
		},
		{
			name:     "三条候选里同协议的那条在最后",
			protocol: ProtocolAnthropicMessages,
			routes: []Route{
				route("chat", ProtocolOpenAIChat),
				route("responses", ProtocolOpenAIResponses),
				route("anthropic", ProtocolAnthropicMessages),
			},
			want: []string{"anthropic", "chat", "responses"},
		},
		{
			// 两条同协议候选之间隔着一条跨协议候选：分区后前者保持声明顺序，后者留在链尾。
			name:     "多条同协议候选保持声明顺序，回退候选也保持声明顺序",
			protocol: ProtocolOpenAIChat,
			routes: []Route{
				route("chat-a", ProtocolOpenAIChat),
				route("anthropic", ProtocolAnthropicMessages),
				route("chat-b", ProtocolOpenAIChat),
			},
			want: []string{"chat-a", "chat-b", "anthropic"},
		},
		{
			name:     "没有同协议候选时顺序原样不动",
			protocol: ProtocolOpenAIResponses,
			routes: []Route{
				route("chat", ProtocolOpenAIChat),
				route("anthropic", ProtocolAnthropicMessages),
			},
			want: []string{"chat", "anthropic"},
		},
		{
			name:     "全部同协议时顺序逐字节不变",
			protocol: ProtocolOpenAIChat,
			routes:   []Route{route("first", ProtocolOpenAIChat), route("second", ProtocolOpenAIChat)},
			want:     []string{"first", "second"},
		},
		{
			name:     "单条候选即使协议不同也不挪动",
			protocol: ProtocolOpenAIChat,
			routes:   []Route{route("anthropic", ProtocolAnthropicMessages)},
			want:     []string{"anthropic"},
		},
		{
			name:     "空候选表返回空结果",
			protocol: ProtocolOpenAIChat,
			want:     nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := joinedUpstreamIDs(tc.routes)
			got := PreferRoutesForProtocol(tc.protocol, tc.routes)

			want := strings.Join(tc.want, "、")
			if actual := joinedUpstreamIDs(got); actual != want {
				t.Errorf("协议 %q 下的候选顺序 = %q，期望 %q", string(tc.protocol), actual, want)
			}
			if len(got) != len(tc.routes) {
				t.Errorf("候选数 = %d，期望与入参相同的 %d 条（一条都不该丢）", len(got), len(tc.routes))
			}
			if actual := joinedUpstreamIDs(tc.routes); actual != before {
				t.Errorf("入参被就地改写成 %q，期望仍是 %q", actual, before)
			}
			for i := range got {
				got[i].UpstreamID = "被调用方改写"
			}
			if actual := joinedUpstreamIDs(tc.routes); actual != before {
				t.Errorf("返回值与入参共享底层数组：改写返回值后入参变成 %q，期望仍是 %q", actual, before)
			}
		})
	}
}
