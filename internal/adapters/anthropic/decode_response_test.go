package anthropic

import (
	"strings"
	"testing"

	"github.com/sumwai/nova/internal/domain"
)

// TestDecodeResponseRejectsNonMessageBody 守护「上游用 2xx 回了非本协议报文」的判定。
//
// 只凭「能解成 JSON 对象」就当作成功是不够的：上游的 404 信封会被解成一条空响应，
// 客户端因此收到「看起来成功但内容为空」的完成响应，比直接报错更难定位。
// 真实案例：智谱在接口路径写错时用 HTTP 200 返回
// {"code":500,"msg":"404 NOT_FOUND","success":false}。
func TestDecodeResponseRejectsNonMessageBody(t *testing.T) {
	bodies := []string{
		`{"code":500,"msg":"404 NOT_FOUND","success":false}`,
		`{"error":{"type":"error","error":{"type":"api_error","message":"boom"}}}`,
		`{"choices":[{"index":0,"message":{"content":"hi"}}]}`,
	}
	adapter := New()
	for _, body := range bodies {
		resp, err := adapter.DecodeResponse([]byte(body))
		if err == nil {
			t.Fatalf("响应体 %s 必须被判为不可解析，实际得到 %+v", body, resp)
		}
		if domain.AsError(err) == nil {
			t.Errorf("响应体 %s 的错误必须是 domain.Error，实际类型为 %T", body, err)
		}
	}
}

// TestDecodeResponseAcceptsMessage 守护正常路径不被上面的校验误伤。
func TestDecodeResponseAcceptsMessage(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"glm-5.3-flash",` +
		`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":3,"output_tokens":2}}`
	resp, err := New().DecodeResponse([]byte(body))
	if err != nil {
		t.Fatalf("正常的 Messages 响应不得被判为不可解析: %v", err)
	}
	if resp.Model != "glm-5.3-flash" {
		t.Errorf("模型名解码不正确: %q", resp.Model)
	}
	if resp.FinishReason != domain.FinishStop {
		t.Errorf("结束原因解码不正确: %q", string(resp.FinishReason))
	}
	if len(resp.Message.Parts) != 1 || resp.Message.Parts[0].Text != "hello" {
		t.Errorf("内容块解码不正确: %+v", resp.Message.Parts)
	}
}

// TestDecodeResponseRejectsMissingContent 守护「type 正确但缺 content」的残缺报文：
// 它是残缺响应，不是「模型什么都没生成」，必须报错并点名缺哪个字段。
func TestDecodeResponseRejectsMissingContent(t *testing.T) {
	_, err := New().DecodeResponse([]byte(`{"type":"message","role":"assistant","model":"m"}`))
	if err == nil {
		t.Fatal("缺少 content 的响应必须报错")
	}
	if !strings.Contains(err.Error(), "content") {
		t.Errorf("错误信息应点名缺失的字段 content，实际为 %q", err.Error())
	}
}
