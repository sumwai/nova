package profileapply

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sumwai/nova/internal/config"
	"github.com/sumwai/nova/internal/domain"
)

// envOf 把一张固定表包成 Getenv，供凭据用例使用。
func envOf(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// applySrc 解析一段配置源码并展开其中的档案引用。
func applySrc(t *testing.T, src string, opts Options) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(src), "Novafile")
	if err != nil {
		t.Fatalf("解析配置失败：%v\n源码：%s", err, src)
	}
	if err := Apply(cfg, opts); err != nil {
		t.Fatalf("展开失败：%v", err)
	}
	return cfg
}

// applyErr 解析并展开一段配置，返回展开错误。
func applyErr(t *testing.T, src string, opts Options) error {
	t.Helper()
	cfg, err := config.Parse([]byte(src), "Novafile")
	if err != nil {
		t.Fatalf("解析配置失败：%v\n源码：%s", err, src)
	}
	return Apply(cfg, opts)
}

// 裸引用把内置档案的端点、模型与凭据补齐，并保留档案 id 作出处。
func TestBareReferenceExpandsBuiltinProfile(t *testing.T) {
	cfg := applySrc(t, "version 1\nprovider opencode-go\n",
		Options{Getenv: envOf(map[string]string{"OPENCODE_API_KEY": "k"})})

	provider := cfg.Providers[0]
	if provider.ProfileRef {
		t.Error("展开后 ProfileRef 应为假")
	}
	if provider.Profile != "opencode-go" {
		t.Errorf("档案 id = %q，期望保留作出处标注", provider.Profile)
	}
	if len(provider.Accounts) != 1 || provider.Accounts[0].APIKey != "k" {
		t.Errorf("账号 = %+v，期望从 auth.env 取到一份凭据", provider.Accounts)
	}
	// 端点顺序取档案里模型声明的优先级：opencode-go 的模型只引用 openai_chat，
	// 未被任何模型引用的 anthropic_messages 按名字接在后面。
	if len(provider.Endpoints) != 2 {
		t.Fatalf("端点数 = %d，期望 2：%+v", len(provider.Endpoints), provider.Endpoints)
	}
	if provider.Endpoints[0].Protocol != domain.ProtocolOpenAIChat {
		t.Errorf("第一个端点协议 = %q，期望 openai_chat", provider.Endpoints[0].Protocol)
	}
	chat := provider.Endpoints[0]
	if provider.Endpoints[1].Protocol != domain.ProtocolAnthropicMessages {
		t.Errorf("第二个端点协议 = %q，期望 anthropic_messages", provider.Endpoints[1].Protocol)
	}
	if chat.Timeout != config.DefaultTimeout {
		t.Errorf("超时 = %v，期望缺省 %v", chat.Timeout, config.DefaultTimeout)
	}
	if len(chat.Models) == 0 {
		t.Error("openai_chat 端点应挂上档案声明的模型")
	}
	// 档案的 auth.header 必须落到渠道上：它高于运行期按协议猜的注入形态。
	if provider.CredentialHeaderStyle != domain.CredentialHeaderAuthorization {
		t.Errorf("凭据注入形态 = %q，期望 authorization", provider.CredentialHeaderStyle)
	}
	// 模型未命中档案里声明的对外名时，选路应能查到它。
	if routes := cfg.Routes("deepseek-flash"); len(routes) == 0 {
		t.Error("展开后 deepseek-flash 应有候选路由")
	}
	// 展开层声明的端点没有 discover：本版不把档案级 discover 带进配置。
	if cfg.DiscoveryCount() != 0 {
		t.Errorf("发现端点数 = %d，期望 0", cfg.DiscoveryCount())
	}
}

// 档案不在任何源里时报错，并说清看过的源与下一步命令。
func TestMissingProfileReportsSourcesAndAdvice(t *testing.T) {
	err := applyErr(t, "version 1\nprovider nope\n", Options{})

	var cerr *config.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("错误类型 = %T，期望 *config.Error", err)
	}
	if cerr.File != "Novafile" || cerr.Line != 2 {
		t.Errorf("定位 = %s:%d，期望 Novafile:2", cerr.File, cerr.Line)
	}
	for _, want := range []string{"nope", "内置源", "nova profiles update"} {
		if !strings.Contains(cerr.Msg, want) {
			t.Errorf("消息 = %q，期望包含 %q", cerr.Msg, want)
		}
	}
}

// 块内写了 api_key 时不再看档案的 auth.env。
func TestManualAPIKeyWinsOverProfileEnv(t *testing.T) {
	cfg := applySrc(t, "version 1\nprovider cmd {\n    profile opencode-go\n    api_key manual\n}\n",
		Options{Getenv: envOf(map[string]string{"OPENCODE_API_KEY": "from-env"})})

	if cfg.Providers[0].Accounts[0].APIKey != "manual" {
		t.Errorf("凭据 = %q，期望块内的 manual", cfg.Providers[0].Accounts[0].APIKey)
	}
}

// 档案 auth.env 指向的环境变量为空时按与占位符展开同一口径报错。
func TestMissingProfileEnvReportsSameWording(t *testing.T) {
	err := applyErr(t, "version 1\nprovider opencode-go\n", Options{Getenv: envOf(nil)})

	var cerr *config.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("错误类型 = %T，期望 *config.Error", err)
	}
	if !strings.Contains(cerr.Msg, "环境变量 OPENCODE_API_KEY 未设置或为空") {
		t.Errorf("消息 = %q，期望与占位符展开同一口径", cerr.Msg)
	}
}

// 同一协议的块内端点覆盖档案地址并记提醒；档案没有的协议只记提醒、保留端点。
func TestManualEndpointsMergeWithWarnings(t *testing.T) {
	cfg := applySrc(t, `
version 1
provider cmd {
    profile opencode-go
    api_key k
    url https://custom.example.com/v1/chat/completions
    model extra
}
`, Options{})

	provider := cfg.Providers[0]
	// 覆盖后的 openai_chat 端点地址来自块内，档案模型与块内模型都在。
	var chat *config.Endpoint
	for i := range provider.Endpoints {
		if provider.Endpoints[i].Protocol == domain.ProtocolOpenAIChat {
			chat = &provider.Endpoints[i]
		}
	}
	if chat == nil {
		t.Fatal("缺少 openai_chat 端点")
	}
	if chat.URL != "https://custom.example.com/v1/chat/completions" {
		t.Errorf("地址 = %q，期望块内地址覆盖档案", chat.URL)
	}
	if !hasModel(*chat, "extra") || !hasModel(*chat, "deepseek-flash") {
		t.Errorf("模型 = %+v，期望块内与档案的模型并存", chat.Models)
	}
	assertWarning(t, cfg, "覆盖")
}

// 块内写了档案没有的协议时，端点保留并记一条提醒。
func TestManualEndpointWithUnknownProtocolWarns(t *testing.T) {
	cfg := applySrc(t, `
version 1
provider cmd {
    profile opencode-go
    api_key k
    url https://custom.example.com/v1/responses
    model r
}
`, Options{})

	var found bool
	for _, endpoint := range cfg.Providers[0].Endpoints {
		if endpoint.Protocol == domain.ProtocolOpenAIResponses {
			found = true
		}
	}
	if !found {
		t.Error("档案没有的协议端点应被保留")
	}
	assertWarning(t, cfg, "不在档案")
}

// 对同一份配置重复展开不改变结果。
func TestApplyIsIdempotent(t *testing.T) {
	cfg := applySrc(t, "version 1\nprovider opencode-go\n",
		Options{Getenv: envOf(map[string]string{"OPENCODE_API_KEY": "k"})})
	before := len(cfg.Providers[0].Endpoints)

	if err := Apply(cfg, Options{}); err != nil {
		t.Fatalf("第二次展开失败：%v", err)
	}
	if got := len(cfg.Providers[0].Endpoints); got != before {
		t.Errorf("端点数 = %d，期望与首次相同 %d", got, before)
	}
}

// 本地源里的档案也能展开；同一端点内重复对外名按展开层口径报错。
func TestLocalSourceExpansionAndDuplicateModel(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "profiles")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatalf("创建档案目录失败：%v", err)
	}
	writeFile(t, filepath.Join(profileDir, "local.yaml"), `
schema: 1
id: local
auth: { header: authorization, scheme: Bearer, env: LOCAL_KEY }
endpoints:
  openai_chat: { url: https://local.example.com/v1/chat/completions, protocol: openai_chat }
models:
  - { id: dup, public: same }
  - { id: dup2, public: same }
`)
	configPath := filepath.Join(dir, "Novafile")
	writeFile(t, configPath, "version 1\nprofiles {\n    source ./profiles\n}\nprovider local\n")

	_, err := Load(configPath, Options{Getenv: envOf(map[string]string{"LOCAL_KEY": "k"})})
	if err == nil {
		t.Fatal("同一端点内重复对外名应报错")
	}
	if !strings.Contains(err.Error(), "重复") {
		t.Errorf("错误 = %v，期望指出模型名重复", err)
	}
}

// 本地源正常展开：端点、模型与凭据都来自磁盘上的档案。
func TestLocalSourceExpandsToConfig(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "profiles")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatalf("创建档案目录失败：%v", err)
	}
	writeFile(t, filepath.Join(profileDir, "local.yaml"), `
schema: 1
id: local
auth: { header: authorization, scheme: Bearer, env: LOCAL_KEY }
endpoints:
  openai_chat: { url: https://local.example.com/v1/chat/completions, protocol: openai_chat }
models:
  - { id: upstream-id, public: alias }
`)
	configPath := filepath.Join(dir, "Novafile")
	writeFile(t, configPath, "version 1\nprofiles {\n    source ./profiles\n}\nprovider local\n")

	cfg, err := Load(configPath, Options{Getenv: envOf(map[string]string{"LOCAL_KEY": "k"})})
	if err != nil {
		t.Fatalf("展开失败：%v", err)
	}
	provider := cfg.Providers[0]
	if provider.ProfileRef || provider.Profile != "local" {
		t.Errorf("渠道 = %+v，期望展开并保留出处", provider)
	}
	if len(provider.Endpoints) != 1 || len(provider.Endpoints[0].Models) != 1 {
		t.Fatalf("端点 = %+v，期望一条端点一个模型", provider.Endpoints)
	}
	model := provider.Endpoints[0].Models[0]
	if model.Name != "alias" || model.Upstream != "upstream-id" {
		t.Errorf("模型 = %+v，期望对外名取 public、上游名取 id", model)
	}
}

// TestLocalSourceExpandsPrice 守护档案的 price / free / price_from 写进渠道模型。
//
// 价格声明是选路排序的输入，展开层必须把它原样搬运，而不是只留一个对外名。
func TestLocalSourceExpandsPrice(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "profiles")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatalf("创建档案目录失败：%v", err)
	}
	writeFile(t, filepath.Join(profileDir, "local.yaml"), `
schema: 1
id: local
auth: { header: authorization, scheme: Bearer, env: LOCAL_KEY }
endpoints:
  openai_chat: { url: https://local.example.com/v1/chat/completions, protocol: openai_chat }
models:
  - id: priced
    price:
      currency: USD
      input_mtok: 0.28
      output_mtok: 0.42
  - id: gratis
    free: true
  - id: alias
    price_from: local/priced
`)
	configPath := filepath.Join(dir, "Novafile")
	writeFile(t, configPath, "version 1\nprofiles {\n    source ./profiles\n}\nprovider local\n")

	cfg, err := Load(configPath, Options{Getenv: envOf(map[string]string{"LOCAL_KEY": "k"})})
	if err != nil {
		t.Fatalf("展开失败：%v", err)
	}
	models := cfg.Providers[0].Endpoints[0].Models
	byName := make(map[string]config.Model, len(models))
	for _, model := range models {
		byName[model.Name] = model
	}

	priced := byName["priced"].Price
	if priced.Key != "local/priced" || priced.Unit == nil {
		t.Fatalf("带单价模型的价格声明 = %+v，期望键 local/priced 且带单价", priced)
	}
	if priced.Unit.InputMTok != 0.28 || priced.Currency != "USD" {
		t.Errorf("单价 = %+v，期望 USD 0.28", priced.Unit)
	}

	if gratis := byName["gratis"].Price; !gratis.Free {
		t.Errorf("显式免费模型的价格声明 = %+v，期望 Free", gratis)
	}

	alias := byName["alias"].Price
	if alias.From != "local/priced" || alias.Unit != nil {
		t.Errorf("price_from 模型的价格声明 = %+v，期望只带引用", alias)
	}
}

// pinnedEndpoint 是一条内置档案展开后应当逐字成立的端点事实。
type pinnedEndpoint struct {
	protocol domain.Protocol
	url      string
	models   []string
}

// TestBuiltinProfilesExpandToPinnedFacts 把三份内置档案各展开一遍，并钉住
// 账号 env、凭据注入形态、端点数、每条端点的地址与模型集合。
//
// 内置档案是随二进制发布的基底事实，地址或模型 id 打错不会有任何编译期信号：
// 展开层的 checkSemantics 只校验 model → endpoint 引用存在，不校验 URL 与对外名重复。
// 这份表是对这些取值的唯一护栏。
func TestBuiltinProfilesExpandToPinnedFacts(t *testing.T) {
	tests := []struct {
		id        string
		env       string
		endpoints []pinnedEndpoint
	}{
		{
			id:  "opencode-go",
			env: "OPENCODE_API_KEY",
			endpoints: []pinnedEndpoint{
				{
					protocol: domain.ProtocolOpenAIChat,
					url:      "https://opencode.ai/zen/go/v1/chat/completions",
					models: []string{
						"deepseek-flash", "deepseek-v4-flash", "deepseek-v4.1-flash",
						"deepseek-v4-pro", "kimi-k3", "glm-5.3", "minimax-m3",
					},
				},
				{protocol: domain.ProtocolAnthropicMessages, url: "https://opencode.ai/zen/go/v1/messages"},
			},
		},
		{
			id:  "sensenova",
			env: "SENSENOVA_API_KEY",
			endpoints: []pinnedEndpoint{
				{
					protocol: domain.ProtocolOpenAIChat,
					url:      "https://token.sensenova.cn/v1/chat/completions",
					models:   []string{"deepseek-flash", "deepseek-v4-flash", "glm-5.2", "kimi-k3", "sensenova-6.8-flash-lite"},
				},
				{
					protocol: domain.ProtocolAnthropicMessages,
					url:      "https://token.sensenova.cn/v1/messages",
					models:   []string{"deepseek-flash", "deepseek-v4-flash"},
				},
			},
		},
		{
			id:  "deepseek-official",
			env: "DEEPSEEK_API_KEY",
			endpoints: []pinnedEndpoint{
				{
					protocol: domain.ProtocolOpenAIChat,
					url:      "https://api.deepseek.com/chat/completions",
					models:   []string{"deepseek-flash", "deepseek-v4-pro"},
				},
				{
					protocol: domain.ProtocolAnthropicMessages,
					url:      "https://api.deepseek.com/anthropic/v1/messages",
					models:   []string{"deepseek-flash", "deepseek-v4-pro"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			cfg := applySrc(t, "version 1\nprovider "+tt.id+"\n",
				Options{Getenv: envOf(map[string]string{tt.env: "k"})})
			provider := cfg.Providers[0]

			if len(provider.Accounts) != 1 || provider.Accounts[0].APIKey != "k" {
				t.Fatalf("账号 = %+v，期望从 %s 取到一份凭据", provider.Accounts, tt.env)
			}
			if provider.CredentialHeaderStyle != domain.CredentialHeaderAuthorization {
				t.Errorf("凭据注入形态 = %q，期望 authorization", provider.CredentialHeaderStyle)
			}
			if len(provider.Endpoints) != len(tt.endpoints) {
				t.Fatalf("端点数 = %d，期望 %d：%+v",
					len(provider.Endpoints), len(tt.endpoints), provider.Endpoints)
			}
			for i, want := range tt.endpoints {
				got := provider.Endpoints[i]
				if got.Protocol != want.protocol {
					t.Errorf("第 %d 条端点协议 = %q，期望 %q", i+1, got.Protocol, want.protocol)
				}
				if got.URL != want.url {
					t.Errorf("%s 端点地址 = %q，期望 %q", want.protocol, got.URL, want.url)
				}
				if names := modelNamesOf(got); !equalStrings(names, want.models) {
					t.Errorf("%s 端点模型 = %v，期望 %v", want.protocol, names, want.models)
				}
			}
		})
	}
}

// TestSameProtocolManualEndpointsDoNotCollapse 守护同协议的多条手写端点不塌成一条。
//
// 块里同时写 profile 与两条同协议端点时，第一条与档案的同协议端点配对并覆盖其地址，
// 第二条作为独立端点保留；只认「该协议的第一条」会让第二条把第一条的地址覆盖掉。
func TestSameProtocolManualEndpointsDoNotCollapse(t *testing.T) {
	cfg := applySrc(t, `
version 1
provider cmd {
    profile opencode-go
    api_key k
    endpoint https://first.example.com/v1/chat/completions {
        model first
    }
    endpoint https://second.example.com/v1/chat/completions {
        model second
    }
}
`, Options{})

	provider := cfg.Providers[0]
	byURL := make(map[string]config.Endpoint, len(provider.Endpoints))
	for _, endpoint := range provider.Endpoints {
		byURL[endpoint.URL] = endpoint
	}
	for _, want := range []struct {
		url   string
		model string
	}{
		{"https://first.example.com/v1/chat/completions", "first"},
		{"https://second.example.com/v1/chat/completions", "second"},
	} {
		endpoint, ok := byURL[want.url]
		if !ok {
			t.Errorf("端点 %s 应被保留，实际端点 = %v", want.url, endpointURLs(provider))
			continue
		}
		if !hasModel(endpoint, want.model) {
			t.Errorf("端点 %s 的模型 = %+v，期望含 %q", want.url, endpoint.Models, want.model)
		}
	}
	// 第二条端点作为配置独有端点保留，提醒不再说它覆盖了档案地址。
	assertWarning(t, cfg, "配置独有")
}

// TestProfileHeadersReachProvider 守护档案声明的静态头落到渠道上。
func TestProfileHeadersReachProvider(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "profiles")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatalf("创建档案目录失败：%v", err)
	}
	writeFile(t, filepath.Join(profileDir, "local.yaml"), `
schema: 1
id: local
auth: { header: x-api-key, env: LOCAL_KEY }
headers:
  X-Tenant: acme
endpoints:
  openai_chat: { url: https://local.example.com/v1/chat/completions, protocol: openai_chat }
models:
  - { id: m }
`)
	configPath := filepath.Join(dir, "Novafile")
	writeFile(t, configPath, "version 1\nprofiles {\n    source ./profiles\n}\nprovider local\n")

	cfg, err := Load(configPath, Options{Getenv: envOf(map[string]string{"LOCAL_KEY": "k"})})
	if err != nil {
		t.Fatalf("展开失败：%v", err)
	}
	provider := cfg.Providers[0]
	if got := provider.Headers.Get("X-Tenant"); got != "acme" {
		t.Errorf("静态头 X-Tenant = %q，期望 acme", got)
	}
	if provider.CredentialHeaderStyle != domain.CredentialHeaderXAPIKey {
		t.Errorf("凭据注入形态 = %q，期望 x-api-key", provider.CredentialHeaderStyle)
	}
}

// TestProfileAuthSchemeRejectedWhenUnrepresentable 守护无法兑现的 auth.scheme 在展开期报错。
//
// 本版只实现 Bearer 方案；把 Basic 静默按 Bearer 注入会把一个必然 401 的请求发出去，
// 而从状态码看不出是方案写错了。
func TestProfileAuthSchemeRejectedWhenUnrepresentable(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "profiles")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatalf("创建档案目录失败：%v", err)
	}
	writeFile(t, filepath.Join(profileDir, "local.yaml"), `
schema: 1
id: local
auth: { header: authorization, scheme: Basic, env: LOCAL_KEY }
endpoints:
  openai_chat: { url: https://local.example.com/v1/chat/completions, protocol: openai_chat }
models:
  - { id: m }
`)
	configPath := filepath.Join(dir, "Novafile")
	writeFile(t, configPath, "version 1\nprofiles {\n    source ./profiles\n}\nprovider local\n")

	_, err := Load(configPath, Options{Getenv: envOf(map[string]string{"LOCAL_KEY": "k"})})
	if err == nil {
		t.Fatal("无法兑现的 auth.scheme 应报错")
	}
	if !strings.Contains(err.Error(), "Bearer") {
		t.Errorf("错误 = %v，期望指出只支持 Bearer", err)
	}
}

// modelNamesOf 取一条端点里模型对外名的列表，供内置档案取值断言使用。
func modelNamesOf(endpoint config.Endpoint) []string {
	names := make([]string, 0, len(endpoint.Models))
	for _, model := range endpoint.Models {
		names = append(names, model.Name)
	}
	return names
}

// endpointURLs 取渠道里全部端点地址，用于失败信息。
func endpointURLs(provider config.Provider) []string {
	urls := make([]string, 0, len(provider.Endpoints))
	for _, endpoint := range provider.Endpoints {
		urls = append(urls, endpoint.URL)
	}
	return urls
}

// equalStrings 报告两个字符串切片逐元素相等。
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// 档案 plans 的静态限制写进渠道的每个账号。
func TestPlanLimitsAttachToAccounts(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "profiles")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatalf("创建档案目录失败：%v", err)
	}
	writeFile(t, filepath.Join(profileDir, "local.yaml"), `
schema: 1
id: local
auth: { header: authorization, scheme: Bearer, env: LOCAL_KEY }
endpoints:
  openai_chat: { url: https://local.example.com/v1/chat/completions, protocol: openai_chat }
models:
  - { id: m }
plans:
  - id: free
    limits:
      - { kind: quota, metric: requests, window: month, tz: "+08:00", limit: 100, used: 100 }
      - { kind: rate, metric: requests, window: 1m, limit: 60 }
`)
	configPath := filepath.Join(dir, "Novafile")
	writeFile(t, configPath, "version 1\nprofiles {\n    source ./profiles\n}\nprovider local\n")

	cfg, err := Load(configPath, Options{Getenv: envOf(map[string]string{"LOCAL_KEY": "k"})})
	if err != nil {
		t.Fatalf("展开失败：%v", err)
	}
	accounts := cfg.Providers[0].Accounts
	if len(accounts) != 1 {
		t.Fatalf("账号数 = %d，期望 1", len(accounts))
	}
	declared := accounts[0].Limits
	if len(declared) != 2 {
		t.Fatalf("声明数 = %d，期望 2：%+v", len(declared), declared)
	}
	if declared[0].Kind != "quota" || declared[0].TZ != "+08:00" {
		t.Errorf("首条声明 = %+v，期望 quota 与 tz 保留", declared[0])
	}
	if declared[0].Limit == nil || *declared[0].Limit != 100 || declared[0].Used == nil || *declared[0].Used != 100 {
		t.Errorf("首条声明的 limit/used = %+v/%+v，期望 100/100", declared[0].Limit, declared[0].Used)
	}
	assertNoWarning(t, cfg, "暂不支持按账号选 plan")
}

// 多个 plan 时取第一个并记一条提醒。
func TestMultiplePlansWarnAndTakeFirst(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "profiles")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatalf("创建档案目录失败：%v", err)
	}
	writeFile(t, filepath.Join(profileDir, "local.yaml"), `
schema: 1
id: local
auth: { header: authorization, scheme: Bearer, env: LOCAL_KEY }
endpoints:
  openai_chat: { url: https://local.example.com/v1/chat/completions, protocol: openai_chat }
models:
  - { id: m }
plans:
  - id: first
    limits:
      - { kind: quota, metric: requests, window: 1m, limit: 10 }
  - id: second
    limits:
      - { kind: quota, metric: requests, window: 1m, limit: 20 }
`)
	configPath := filepath.Join(dir, "Novafile")
	writeFile(t, configPath, "version 1\nprofiles {\n    source ./profiles\n}\nprovider local\n")

	cfg, err := Load(configPath, Options{Getenv: envOf(map[string]string{"LOCAL_KEY": "k"})})
	if err != nil {
		t.Fatalf("展开失败：%v", err)
	}
	assertWarning(t, cfg, "暂不支持按账号选 plan")
	declared := cfg.Providers[0].Accounts[0].Limits
	if len(declared) != 1 || declared[0].Limit == nil || *declared[0].Limit != 10 {
		t.Errorf("声明 = %+v，期望取第一个 plan 的 limit=10", declared)
	}
}

// hasModel 报告一条端点里是否有某个对外模型名。
func hasModel(endpoint config.Endpoint, name string) bool {
	for _, model := range endpoint.Models {
		if model.Name == name {
			return true
		}
	}
	return false
}

// assertWarning 断言配置里有一条含指定片段的提醒。
func assertWarning(t *testing.T, cfg *config.Config, want string) {
	t.Helper()
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning.Msg, want) {
			return
		}
	}
	t.Errorf("提醒 = %+v，期望含 %q", cfg.Warnings, want)
}

// assertNoWarning 断言配置里没有含指定片段的提醒。
func assertNoWarning(t *testing.T, cfg *config.Config, want string) {
	t.Helper()
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning.Msg, want) {
			t.Errorf("提醒 = %+v，不应含 %q", cfg.Warnings, want)
		}
	}
}

// writeFile 写一个测试用文件。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写文件 %s 失败：%v", path, err)
	}
}

// localRelaySource 造一个只含一份档案的本地源，返回配置目录。
//
// api_pattern 指向 relay.example.com，供「手写地址自动认档案」的用例使用。
func localRelaySource(t *testing.T, modelYAML string) string {
	t.Helper()
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "profiles")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatalf("创建档案目录失败：%v", err)
	}
	writeFile(t, filepath.Join(profileDir, "relay.yaml"), `
schema: 1
id: relay
name: Relay
updated_at: 2026-09-26T00:00:00Z
api_pattern: '^https://relay\.example\.com/'
auth: { header: authorization, scheme: Bearer, env: RELAY_KEY }
endpoints:
  openai_chat: { url: https://relay.example.com/v1/chat/completions, protocol: openai_chat }
models:
`+modelYAML)
	return dir
}

// 手写渠道的地址命中唯一档案时，价格与模型集合随档案接入。
func TestManualProviderAutoDetectsProfileByURL(t *testing.T) {
	dir := localRelaySource(t, "  - { id: m, price_from: relay/m-price }\n")
	configPath := filepath.Join(dir, "Novafile")
	writeFile(t, configPath, `version 1
profiles {
    source ./profiles
}
provider myrelay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    model m
}
`)

	cfg, err := Load(configPath, Options{})
	if err != nil {
		t.Fatalf("展开失败：%v", err)
	}
	provider := cfg.Providers[0]
	if provider.Profile != "relay" {
		t.Fatalf("档案 id = %q，期望按地址认到 relay", provider.Profile)
	}
	if provider.ProfileRef {
		t.Error("展开后 ProfileRef 应为假")
	}
	if len(provider.Endpoints) != 1 || len(provider.Endpoints[0].Models) != 1 {
		t.Fatalf("端点 = %+v，期望一条端点一个同名模型", provider.Endpoints)
	}
	if got := provider.Endpoints[0].Models[0].Price.Key; got != "relay/m" {
		t.Errorf("价格键 = %q，期望档案声明 relay/m", got)
	}
}

// 地址同时命中两份档案时报错并列候选，不静默取一份。
func TestManualProviderAmbiguousPatternReportsError(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "profiles")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatalf("创建档案目录失败：%v", err)
	}
	for _, id := range []string{"relay-a", "relay-b"} {
		writeFile(t, filepath.Join(profileDir, id+".yaml"), `
schema: 1
id: `+id+`
api_pattern: '^https://relay\.example\.com/'
auth: { header: authorization, scheme: Bearer, env: RELAY_KEY }
endpoints:
  openai_chat: { url: https://relay.example.com/v1/chat/completions, protocol: openai_chat }
models:
  - { id: m }
`)
	}
	configPath := filepath.Join(dir, "Novafile")
	writeFile(t, configPath, `version 1
profiles {
    source ./profiles
}
provider myrelay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    model m
}
`)

	_, err := Load(configPath, Options{})
	if err == nil {
		t.Fatal("命中两份档案时应报错")
	}
	for _, want := range []string{"relay-a", "relay-b", "profile"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误 = %v，期望含 %q", err, want)
		}
	}
}

// 地址不命中任何档案时保持手写渠道原样，不补价格也不报错。
func TestManualProviderWithoutPatternStaysManual(t *testing.T) {
	dir := localRelaySource(t, "  - { id: m, price_from: relay/m-price }\n")
	configPath := filepath.Join(dir, "Novafile")
	writeFile(t, configPath, `version 1
profiles {
    source ./profiles
}
provider myrelay {
    api_key k
    url https://other.example.com/v1/chat/completions
    model m
}
`)

	cfg, err := Load(configPath, Options{})
	if err != nil {
		t.Fatalf("展开失败：%v", err)
	}
	provider := cfg.Providers[0]
	if provider.Profile != "" {
		t.Errorf("档案 id = %q，期望未命中时保持为空", provider.Profile)
	}
	if len(provider.Endpoints) != 1 || len(provider.Endpoints[0].Models) != 1 {
		t.Fatalf("端点 = %+v，期望手写端点原样保留", provider.Endpoints)
	}
	if provider.Endpoints[0].Models[0].Price.Key != "" {
		t.Errorf("手写模型不应被补上价格：%+v", provider.Endpoints[0].Models[0].Price)
	}
}

// 档案计划的到期时刻写进账号额度声明，供装配层剔除已到期的计划。
func TestPlanExpiresAtExpandsToAccountLimits(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "profiles")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatalf("创建档案目录失败：%v", err)
	}
	writeFile(t, filepath.Join(profileDir, "trial.yaml"), `
schema: 1
id: trial
auth: { header: authorization, scheme: Bearer, env: TRIAL_KEY }
endpoints:
  openai_chat: { url: https://trial.example.com/v1/chat/completions, protocol: openai_chat }
models:
  - { id: m }
plans:
  - id: trial
    expires_at: 2026-10-03T00:00:00Z
    limits:
      - { kind: quota, metric: usd, window: 5h, limit: 10 }
`)
	configPath := filepath.Join(dir, "Novafile")
	writeFile(t, configPath, "version 1\nprofiles {\n    source ./profiles\n}\nprovider trial\n")

	cfg, err := Load(configPath, Options{Getenv: envOf(map[string]string{"TRIAL_KEY": "k"})})
	if err != nil {
		t.Fatalf("展开失败：%v", err)
	}
	declared := cfg.Providers[0].Accounts[0].Limits
	if len(declared) != 1 {
		t.Fatalf("额度声明 = %+v，期望一条", declared)
	}
	if declared[0].ExpiresAt != "2026-10-03T00:00:00Z" {
		t.Errorf("expires_at = %q，期望随计划写入账号额度", declared[0].ExpiresAt)
	}
}

// 手写模型与档案同名但上游名不同时报错，不静默选一边。
func TestManualModelUpstreamConflictReportsError(t *testing.T) {
	dir := localRelaySource(t, "  - { id: upstream-x, public: m }\n")
	configPath := filepath.Join(dir, "Novafile")
	writeFile(t, configPath, `version 1
profiles {
    source ./profiles
}
provider myrelay {
    api_key k
    url https://relay.example.com/v1/chat/completions
    model m m
}
`)

	_, err := Load(configPath, Options{})
	if err == nil {
		t.Fatal("同名模型指向上游不同名时应报错")
	}
	if !strings.Contains(err.Error(), "上游") {
		t.Errorf("错误 = %v，期望指出上游模型名冲突", err)
	}
}
