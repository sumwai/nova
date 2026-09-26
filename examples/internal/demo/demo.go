// Package demo 把「一个示例要演示什么」写成可执行、可打印的脚本。
//
// 示例的校验（examples/internal/harness）与交互式运行（examples/run）都走这里，
// 因此两处看到的**是同一份事实**：期望什么、实际观察到了什么、判定成不成立。
// 分成两份的代价不是多写几行，而是「文档说会这样、校验查的是那样」这种漂移。
//
// 一步（Step）由三件事构成：标题（在证明什么）、期望（应当观察到什么）、
// 以及执行体（发请求、对事实下判定、给出实际观察到的事实行）。判定不成立时返回错误，
// 错误与事实一起进报告——两者的排版单位是同一块。
package demo

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/nova/examples/internal/chatmock"
	"github.com/sumwai/nova/examples/internal/stack"
)

// Step 是示例里的一步。
type Step struct {
	// Title 是这一步在证明什么。
	Title string
	// Expect 是「应当观察到什么」，一句给人看的话。
	Expect string
	// Do 执行这一步，返回实际观察到的事实行；返回错误即判定不成立。
	//
	// 判定写在 Do 里而不是另开一个断言函数：请求与「为什么这么请求」是一个整体，
	// 拆成两处之后，读代码的人要在两个函数之间来回对照才知道这一步在验什么。
	Do func(c *Client) ([]string, error)
}

// Scenario 是一个示例的完整脚本。
type Scenario struct {
	// Name 是示例名，也是配置目录名。
	Name string
	// Config 是配置文件名；空时取 Novafile。
	Config string
	// Env 是运行这个示例需要的环境变量，例如 {env.NAME} 对应的取值。
	Env map[string]string
	// NoServe 为真时不启动 nova 服务，只构建二进制并跑子命令。
	// 接真实上游的示例用它：那份配置只能被校验，不能被跑通。
	NoServe bool
	// Steps 按顺序执行，前一步失败即停：后面的步骤常建立在前一步产生的事实上，
	// 继续跑只会刷出一串同源错误。
	Steps []Step
}

// Result 是一步的执行结果。
type Result struct {
	Step  Step
	Facts []string
	Err   error
}

// Passed 报告这一步是否通过。
func (r Result) Passed() bool { return r.Err == nil }

// Client 是示例访问网关与模拟上游的入口。
//
// 它只返回错误、不直接让测试失败：同一份代码要在校验（失败即测试失败）与
// 交互式运行（失败即打印出来、停下让人看）两种场合工作。
type Client struct {
	// BaseURL 是客户端请求进来的地址；NoServe 的场景下为空。
	BaseURL string
	// Key 是客户端凭据。
	Key string
	// Mock 是模拟上游；NoServe 的场景下为 nil。
	Mock *chatmock.Mock
	// Stack 是服务进程；NoServe 的场景下为 nil。
	Stack *stack.Stack
	// CLI 用来跑 nova 子命令。
	CLI CLI

	// checkRun 是 NoServe 场景下的子命令发起器，供 WithEnv 派生一份换了环境的实例。
	checkRun *stack.CheckRun

	http *http.Client
}

// CLI 是跑 nova 子命令的能力，由 stack.Stack 与 stack.CheckRun 实现。
type CLI interface {
	CLI(args ...string) (string, int)
}

// Start 起脚本所需的环境，返回 Client 与关闭函数。
//
// out 是 nova 日志的落点；nil 表示只留在内存里（校验用，失败时再打）。
func Start(s Scenario, dir string, out io.Writer) (*Client, func(), error) {
	configName := s.Config
	if configName == "" {
		configName = "Novafile"
	}
	configPath := filepath.Join(dir, configName)

	if s.NoServe {
		temp, err := os.MkdirTemp("", "nova-example-check-")
		if err != nil {
			return nil, nil, err
		}
		binary, err := stack.Build(temp)
		if err != nil {
			_ = os.RemoveAll(temp)
			return nil, nil, err
		}
		checker := &stack.CheckRun{Binary: binary, Dir: dir, Extra: s.Env}
		client := &Client{Key: stack.DefaultKey, CLI: checker, checkRun: checker, http: httpClient()}
		return client, func() { _ = os.RemoveAll(temp) }, nil
	}

	st, err := stack.Start(stack.Options{Config: configPath, ExtraEnv: s.Env, Output: out})
	if err != nil {
		return nil, nil, err
	}
	client := &Client{
		BaseURL: st.BaseURL,
		Key:     st.Key,
		Mock:    st.Mock(),
		Stack:   st,
		CLI:     st,
		http:    httpClient(),
	}
	return client, st.Close, nil
}

// Execute 依次执行各步，返回逐步结果；前一步失败即停。
func Execute(c *Client, steps []Step) []Result {
	results := make([]Result, 0, len(steps))
	for _, step := range steps {
		facts, err := step.Do(c)
		results = append(results, Result{Step: step, Facts: facts, Err: err})
		if err != nil {
			break
		}
	}
	return results
}

// Report 把一步的结果排版成给人与给测试读的块。
//
// 期望与实际并排：只打「失败」而不打「期望什么」的话，读的人还得回去翻代码；
// 两者放在一起时，不一致本身就是结论。序号由调用方给，使报告与脚本一一对应。
func (r Result) Report(index int) string {
	var sb strings.Builder
	mark := "✓"
	if !r.Passed() {
		mark = "✗"
	}
	fmt.Fprintf(&sb, "%s %d. %s\n", mark, index, r.Step.Title)
	if r.Step.Expect != "" {
		sb.WriteString("     期望  " + r.Step.Expect + "\n")
	}
	for _, fact := range r.Facts {
		sb.WriteString("     实际  " + fact + "\n")
	}
	if r.Err != nil {
		sb.WriteString("     失败  " + r.Err.Error() + "\n")
	}
	return sb.String()
}

// Print 把整份报告写到 out。
func Print(out io.Writer, results []Result) {
	for i, result := range results {
		fmt.Fprintf(out, "\n%s", result.Report(i+1))
	}
}

// WithEnv 派生一个换了环境变量的子命令发起器，供「这一步要换一份环境」的步骤使用，
// 例如断言某个 {env.NAME} 缺失时加载会失败。空值表示**不设**该变量。
//
// 只对 NoServe 的场景有效：服务进程的环境在启动时就定了，改了也不会作用到已起的进程上，
// 因此那种场景下原样返回现有的发起器，而不是给出一份看起来生效、实际没用的环境。
func (c *Client) WithEnv(overrides map[string]string) CLI {
	if c.checkRun == nil {
		return c.CLI
	}
	derived := *c.checkRun
	derived.Extra = make(map[string]string, len(c.checkRun.Extra)+len(overrides))
	for name, value := range c.checkRun.Extra {
		derived.Extra[name] = value
	}
	for name, value := range overrides {
		derived.Extra[name] = value
	}
	return &derived
}

// Response 是一次客户端请求的结果。
type Response struct {
	Method string
	Path   string
	Status int
	Header http.Header
	Body   string
}

// Fact 把一次请求压成一行事实。
//
// 流式按「帧数」而不是整段正文说明：一整条 SSE 贴进报告会淹掉真正要看的东西
// （状态码、命中哪条渠道、上游收到的模型名对不对）。非流式给正文片段，因为那里
// 正是「回显的上游模型名」这类断言的落点。
func (r Response) Fact() string {
	line := r.Method + " " + r.Path + " → " + strconv.Itoa(r.Status)
	if strings.Contains(r.Header.Get("Content-Type"), "event-stream") {
		return line + "  text/event-stream，" + strconv.Itoa(frameCount(r.Body)) + " 帧"
	}
	summary := compact(r.Body)
	if len(summary) > bodyPreviewRunes {
		summary = truncate(summary, bodyPreviewRunes) + "…"
	}
	if summary == "" {
		return line
	}
	return line + "  " + summary
}

// bodyPreviewRunes 是报告里正文片段的长度上限，按字符（rune）而不是字节算。
//
// 按字节切会把 UTF-8 从中间断开，而错误体里就有中文（见 chatmock 的故障响应）。
const bodyPreviewRunes = 96

// truncate 把一段文本截到 n 个字符，不把多字节字符切开。
func truncate(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n])
}

// Expect 断言状态码与正文含某个子串。body 为空时只查状态码。
func (r Response) Expect(status int, body string) error {
	if r.Status != status {
		return fmt.Errorf("状态码 = %d，期望 %d；响应体：%s", r.Status, status, compact(r.Body))
	}
	if body != "" && !strings.Contains(r.Body, body) {
		return fmt.Errorf("响应体不含 %q；响应体：%s", body, compact(r.Body))
	}
	return nil
}

// ExpectAbsent 断言正文不含某个子串。
//
// 「目录里不该出现什么」与「响应里应当出现什么」是两类断言，都要能表达：
// 只断言前半句时，一个把所有模型都暴露出去的实现会照样通过。
func (r Response) ExpectAbsent(body string) error {
	if strings.Contains(r.Body, body) {
		return fmt.Errorf("响应体不该含 %q；响应体：%s", body, r.Body)
	}
	return nil
}

// Request 是一次要发的请求。凭据头形态由 Headers 表达。
type Request struct {
	Method  string
	Path    string
	Body    string
	Headers map[string]string
	// NoKey 为真时不带客户端凭据，用来断言鉴权确实生效。
	NoKey bool
}

// Do 发一次请求并读完整正文。
//
// 请求失败（连不上、读不出）返回错误而不是让调用方拿零值继续：
// 那种失败与「断言不成立」是两件事，前者说明环境不对。
func (c *Client) Do(req Request) (Response, error) {
	method := req.Method
	if method == "" {
		method = http.MethodPost
	}
	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	httpReq, err := http.NewRequest(method, c.BaseURL+req.Path, body)
	if err != nil {
		return Response{}, err
	}
	for name, value := range req.Headers {
		httpReq.Header.Set(name, value)
	}
	if !req.NoKey {
		httpReq.Header.Set("Authorization", "Bearer "+c.Key)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("请求 %s %s 失败：%w", method, req.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, err
	}
	return Response{
		Method: method,
		Path:   req.Path,
		Status: resp.StatusCode,
		Header: resp.Header,
		Body:   string(payload),
	}, nil
}

// Get 发一个带凭据的 GET。
func (c *Client) Get(path string) (Response, error) {
	return c.Do(Request{Method: http.MethodGet, Path: path})
}

// GetAnthropic 发一个带 anthropic-version 的 GET。
func (c *Client) GetAnthropic(path string) (Response, error) {
	return c.Do(Request{Method: http.MethodGet, Path: path, Headers: map[string]string{
		"anthropic-version": "2023-06-01",
	}})
}

// Post 发一个用 Authorization 提交凭据的 POST。
func (c *Client) Post(path, body string) (Response, error) {
	return c.Do(Request{Path: path, Body: body, Headers: jsonHeaders()})
}

// PostAnthropic 发一个 Anthropic 客户端形态的 POST：带 anthropic-version 头。
func (c *Client) PostAnthropic(path, body string) (Response, error) {
	return c.Do(Request{Path: path, Body: body, Headers: map[string]string{
		"Content-Type":      "application/json",
		"anthropic-version": "2023-06-01",
	}})
}

// PostGemini 发一个 Gemini 客户端形态的 POST：凭据用 x-goog-api-key 提交。
func (c *Client) PostGemini(path, body string) (Response, error) {
	return c.Do(Request{Path: path, Body: body, Headers: map[string]string{
		"Content-Type":   "application/json",
		"x-goog-api-key": c.Key,
	}})
}

func jsonHeaders() map[string]string {
	return map[string]string{"Content-Type": "application/json"}
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// Fail 造一个判定错误，带上期望值与实际值。
//
// 判定失败时把两侧一起给出，是本包对错误信息的统一要求：只说「不对」的报告
// 会让读的人回去翻代码才能知道哪里不对。
func Fail(what, got, want string) error {
	return fmt.Errorf("%s = %q，期望 %q", what, got, want)
}

// Facts 把一次响应与它引起的上游尝试合成事实行。
func Facts(resp Response, attempts []chatmock.Request) []string {
	facts := []string{resp.Fact()}
	return append(facts, AttemptFacts(attempts)...)
}

// Reset 清空模拟上游收到的请求记录：下一步通常只关心自己产生的那几次尝试。
func (c *Client) Reset() {
	if c.Mock != nil {
		c.Mock.Reset()
	}
}

// Attempts 返回上游模型名为 model 的尝试记录，按发生顺序。
func (c *Client) Attempts(model string) []chatmock.Request {
	if c.Mock == nil {
		return nil
	}
	var matched []chatmock.Request
	for _, req := range c.Mock.Requests() {
		if req.Model == model {
			matched = append(matched, req)
		}
	}
	return matched
}

// AllAttempts 返回全部尝试记录。
func (c *Client) AllAttempts() []chatmock.Request {
	if c.Mock == nil {
		return nil
	}
	return c.Mock.Requests()
}

// AttemptFacts 把尝试记录逐条压成事实行。
//
// 每条都带序号、协议、路径、上游模型名、凭据与故障开关：回退链的顺序、
// 账号池用了哪条账号、地址末段的动作有没有换，这些是「过程」，只有记录能回答；
// 客户端看到的响应只反映最终结果。
func AttemptFacts(attempts []chatmock.Request) []string {
	facts := make([]string, 0, len(attempts))
	for i, attempt := range attempts {
		stream := "非流式"
		if attempt.Stream {
			stream = "流式"
		}
		line := fmt.Sprintf("上游 #%d  %s %s  model=%s  key=%s  经 %s  %s",
			i+1, attempt.Protocol, attempt.Path, attempt.Model, attempt.Key, attempt.CredentialHeader, stream)
		if attempt.Fault != "" {
			line += "  故障=" + attempt.Fault
		}
		facts = append(facts, line)
	}
	return facts
}

// Keys 返回尝试记录里的凭据，按顺序。
func Keys(attempts []chatmock.Request) []string {
	keys := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		keys = append(keys, attempt.Key)
	}
	return keys
}

// ExpectAttempts 断言尝试条数。
func ExpectAttempts(attempts []chatmock.Request, want int) error {
	if len(attempts) != want {
		return fmt.Errorf("上游尝试 %d 次，期望 %d 次", len(attempts), want)
	}
	return nil
}

// ExpectKeys 断言尝试顺序上的凭据。
func ExpectKeys(attempts []chatmock.Request, want ...string) error {
	got := Keys(attempts)
	if len(got) != len(want) {
		return fmt.Errorf("尝试 %d 次，期望 %d 次：凭据序列 = %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("凭据序列 = %v，期望 %v", got, want)
		}
	}
	return nil
}

// Since 取一段追加输出：日志是持续追加的，调用前后两次读取的差就是这次调用产生的记录。
//
// 用它而不是「最后 N 行」：一次调用可能产生多行（访问主行加每条上游尝试），
// 而它之前那些行属于别的调用。
func Since(before, after string) string {
	if strings.HasPrefix(after, before) {
		return strings.TrimSpace(after[len(before):])
	}
	return after
}

// LineContaining 从一段输出里取第一条含 needle 的行，找不到返回空串。
//
// 用它把「输出里的某一行」既当断言对象又当事实行：只在失败时给出整段输出，
// 会让人看不出到底打印了什么。
func LineContaining(output, needle string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, needle) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// CLIFact 把一次子命令压成事实行：退出码加上输出的首行。
func CLIFact(args []string, output string, code int) string {
	first := ""
	for _, line := range strings.Split(output, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			first = trimmed
			break
		}
	}
	return fmt.Sprintf("nova %s → 退出码 %d  %s", strings.Join(args, " "), code, first)
}

// compact 把正文压成一行：JSON 里的缩进与换行对报告没有信息量。
func compact(body string) string {
	return strings.Join(strings.Fields(body), " ")
}

// frameCount 数 SSE 正文里的 data 帧数。
func frameCount(body string) int {
	count := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data:") {
			count++
		}
	}
	return count
}
