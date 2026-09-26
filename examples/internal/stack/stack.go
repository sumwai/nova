// Package stack 起一个示例所需的两个进程：模拟上游与 nova。
//
// 它不含断言、也不依赖 testing：示例校验（examples/internal/harness）与交互式运行
// （examples/run）共用它，因此「怎么起、怎么等就绪、怎么收尾」只有一份实现。
// 分成两份的代价不是多写几行，而是两边会在某个时刻对「就绪了没有」给出不同答案。
//
// 配置是唯一的端口来源：模拟上游地址与两个监听地址都从示例的 Novafile 里读出来，
// 因此「测试里写 18081、配置里写 18082」这类不一致没有发生的余地。
// 代价是示例配置里这几条指令必须写字面量（不写 {env}），这本来就是示例该有的样子。
package stack

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sumwai/nova/examples/internal/chatmock"
)

const (
	// DefaultKey 是注入 NOVA_CLIENT_KEY 的缺省取值。配置里写 {env.NOVA_CLIENT_KEY}，
	// 示例因此不在仓库里留一个明文凭据（哪怕它只是个演示值）。
	DefaultKey = "example-client-key"
	// readyTimeout 是等待 nova 就绪的上限。配置写错时进程会立刻退出，通常用不到它。
	readyTimeout = 15 * time.Second
	// stopTimeout 是收到 TERM 之后等待收尾的上限。
	stopTimeout = 10 * time.Second
)

// Config 是从示例配置里读出的运行事实。
type Config struct {
	// Path 是配置文件的绝对路径。
	Path string
	// Listen 是客户端请求进来的地址，Admin 是 nova reload 投递配置重载的地址。
	Listen string
	Admin  string
	// MockAddrs 是配置里指向本机的上游地址（去重、按出现顺序），也就是要起的模拟上游。
	MockAddrs []string
}

// ParseConfig 从示例配置里读出监听地址与本机上游地址。
//
// 只认四种指令：listen、admin、url、端点块头 endpoint 的取值，以及 discover 后
// 可选的清单地址。判定「指向本机」按主机名，因此 127.0.0.1 与 localhost 都算。
//
// 这是一段刻意很窄的解析：它读的是示例自己的配置，格式由 examples/ 控制，
// 因此不引入配置包的内部结构，也不打算容忍任意的 Novafile 写法——
// 读不到 listen 或 admin 时直接报错，而不是猜一个缺省端口。
func ParseConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{Path: absolute}
	seen := make(map[string]struct{})
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		value := ""
		if len(fields) > 1 {
			value = strings.Trim(fields[1], `"`)
		}
		switch fields[0] {
		case "listen":
			if cfg.Listen == "" {
				cfg.Listen = value
			}
		case "admin":
			if cfg.Admin == "" {
				cfg.Admin = value
			}
		case "url", "endpoint", "discover":
			addr, ok := localAddr(value)
			if !ok {
				continue
			}
			if _, dup := seen[addr]; dup {
				continue
			}
			seen[addr] = struct{}{}
			cfg.MockAddrs = append(cfg.MockAddrs, addr)
		}
	}

	if cfg.Listen == "" {
		return Config{}, fmt.Errorf("%s：没有找到 listen 指令", path)
	}
	if cfg.Admin == "" {
		return Config{}, fmt.Errorf("%s：没有找到 admin 指令", path)
	}
	return cfg, nil
}

// localAddr 判断一个取值是不是指向本机的地址，是则返回 host:port。
func localAddr(value string) (string, bool) {
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return "", false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Port() == "" {
		return "", false
	}
	switch parsed.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return net.JoinHostPort(parsed.Hostname(), parsed.Port()), true
	}
	return "", false
}

// Options 描述要起什么。
type Options struct {
	// Config 是配置文件路径。
	Config string
	// Models 是模拟上游的清单端点返回的模型 id；空时取 chatmock.DefaultModels。
	Models []string
	// Key 是客户端凭据取值；空时取 DefaultKey。
	Key string
	// ExtraEnv 是额外注入的环境变量。
	ExtraEnv map[string]string
	// Output 是 nova 进程 stdout/stderr 的落点；nil 时只留在内存里。
	Output io.Writer
}

// Stack 是一个跑起来的示例：模拟上游、nova 进程，以及访问它们所需的地址。
type Stack struct {
	BaseURL  string
	AdminURL string
	Key      string
	// Binary 是本次构建出的 nova 可执行文件；子命令用它。
	Binary string
	// ConfigPath 是配置文件的绝对路径。
	ConfigPath string
	// Dir 是 nova 进程的工作目录，也就是配置文件所在目录。
	Dir string

	logs    *LogBuffer
	env     []string
	cmd     *exec.Cmd
	done    chan struct{}
	tempDir string
	mock    *chatmock.Mock
	servers []*http.Server
	stop    sync.Once
	mu      sync.Mutex
}

// Start 起模拟上游与 nova，并等到存活探针可用；任何一步失败都会把自己收干净再返回错误。
func Start(opts Options) (*Stack, error) {
	key := opts.Key
	if key == "" {
		key = DefaultKey
	}
	cfg, err := ParseConfig(opts.Config)
	if err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp("", "nova-example-")
	if err != nil {
		return nil, err
	}
	binary, err := Build(tempDir)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, err
	}

	st := &Stack{
		BaseURL:    "http://" + cfg.Listen,
		AdminURL:   "http://" + cfg.Admin,
		Key:        key,
		Binary:     binary,
		ConfigPath: cfg.Path,
		Dir:        filepath.Dir(cfg.Path),
		logs:       &LogBuffer{},
		tempDir:    tempDir,
		mock:       chatmock.New(opts.Models),
	}
	if err := st.startMocks(cfg.MockAddrs); err != nil {
		st.Close()
		return nil, err
	}
	if err := st.startNova(opts); err != nil {
		st.Close()
		return nil, err
	}
	if err := st.waitReady(); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}

// Build 把 nova 构建到 destDir，返回可执行文件路径。
//
// 构建的是真二进制而不是进程内调用 internal/gateway：示例展示的是
// 「配置文件 → 命令行 → HTTP」这条链路，进程内调用会把前两段排除在外。
func Build(destDir string) (string, error) {
	root, err := RepoRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	binary := filepath.Join(destDir, "nova")
	build := exec.Command("go", "build", "-o", binary, "./cmd/nova")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("构建 nova 失败：%w\n%s", err, output)
	}
	return binary, nil
}

// RepoRoot 从当前目录向上找到 go.mod，给出模块根。
//
// 不用相对路径推算：调用方的工作目录可能是仓库根、也可能是某个示例目录。
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("从当前目录向上没有找到 go.mod")
		}
		dir = parent
	}
}

// startMocks 在每个指定地址上起一个监听，但只用一个模拟上游：
// 多条渠道指向同一个模拟上游是常见形态，而「所有上游请求记在同一份记录里」
// 正是断言需要的形状。地址被占用时明确说是端口冲突：那种失败与「断言不成立」是两件事。
func (s *Stack) startMocks(addrs []string) error {
	for _, addr := range addrs {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("模拟上游无法监听 %s：%w\n"+
				"该端口被占用时请先停掉占用它的进程：另一个示例、或手工起着的一份 chatmock", addr, err)
		}
		server := &http.Server{Handler: s.mock}
		s.mu.Lock()
		s.servers = append(s.servers, server)
		s.mu.Unlock()
		go func() { _ = server.Serve(listener) }()
	}
	return nil
}

// Mock 返回本次运行使用的模拟上游。
func (s *Stack) Mock() *chatmock.Mock { return s.mock }

// startNova 启动 nova，把它的输出留在内存里，需要时同时转发给调用方。
func (s *Stack) startNova(opts Options) error {
	// 状态与配置目录都落到临时目录：开发机上通常有一个真的 nova 在跑，
	// 它用的是 ~/.local/state/nova/stats.db，不隔离就会把示例流量写进那份真实统计里。
	env := map[string]string{
		"XDG_STATE_HOME":  filepath.Join(s.tempDir, "state"),
		"XDG_CONFIG_HOME": filepath.Join(s.tempDir, "config"),
		"NOVA_CLIENT_KEY": s.Key,
		// 日志着色是运行时判定，示例里去掉它，看到的就只是文本本身。
		"NO_COLOR": "1",
	}
	for name, value := range opts.ExtraEnv {
		env[name] = value
	}
	s.env = withEnv(os.Environ(), env)

	cmd := exec.Command(s.Binary, "run", "-c", s.ConfigPath)
	cmd.Dir = s.Dir
	cmd.Env = s.env
	if opts.Output != nil {
		cmd.Stdout = io.MultiWriter(s.logs, opts.Output)
		cmd.Stderr = io.MultiWriter(s.logs, opts.Output)
	} else {
		cmd.Stdout = s.logs
		cmd.Stderr = s.logs
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	s.cmd = cmd
	s.done = make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(s.done)
	}()
	return nil
}

// waitReady 轮询存活探针，直到 nova 就绪。
//
// 用探针而不是固定等待：固定等待要么让每次运行都慢，要么在慢机器上偶发失败。
// 探针不经鉴权（网关的探活只回答「进程是否在线」），因此这里不需要凭据。
func (s *Stack) waitReady() error {
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(readyTimeout)
	for {
		select {
		case <-s.done:
			return fmt.Errorf("nova 在就绪前退出。日志：\n%s", s.logs.String())
		default:
		}
		resp, err := client.Get(s.BaseURL + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nova 未在 %s 内就绪（%s/healthz）。日志：\n%s", readyTimeout, s.BaseURL, s.logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Close 先给 nova 发 TERM 让它走正常收尾，超时才杀，然后关掉模拟上游并删掉临时目录。
//
// 与生产接法一致（systemd 的 ExecStop 也是 TERM）：直接 Kill 会把
// 「收尾有没有挂住」这类问题从示例里抹掉。
func (s *Stack) Close() {
	s.stop.Do(func() {
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-s.done:
			case <-time.After(stopTimeout):
				_ = s.cmd.Process.Kill()
				<-s.done
			}
		}
		s.mu.Lock()
		for _, server := range s.servers {
			_ = server.Close()
		}
		s.servers = nil
		s.mu.Unlock()
		if s.tempDir != "" {
			_ = os.RemoveAll(s.tempDir)
		}
	})
}

// Log 返回 nova 截至目前写出的全部输出。
func (s *Stack) Log() string { return s.logs.String() }

// CLI 在同一个环境下跑一次 nova 子命令，返回合并输出与退出码。
//
// 子命令与服务进程看到完全同一份环境：「服务能跑、命令行读不到配置」这种差异
// 不该出现在示例里。子命令根本没跑起来时退出码为 -1，调用方据此区分它与命令自身的失败。
func (s *Stack) CLI(args ...string) (string, int) {
	cmd := exec.Command(s.Binary, args...)
	cmd.Dir = s.Dir
	cmd.Env = s.env
	output, err := cmd.CombinedOutput()
	if err == nil {
		return string(output), 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return string(output) + err.Error(), -1
	}
	return string(output), exitErr.ExitCode()
}

// CheckRun 只跑 nova 子命令，不起服务。
//
// 面向那类「接真实上游、因而不能被跑通」的示例：它的配置只能被校验。
// 它不监听端口，因此不会与开发机上正在运行的 nova 抢同一个 listen。
type CheckRun struct {
	// Binary 是要跑的 nova 可执行文件。
	Binary string
	// Dir 是工作目录：配置文件的相对路径相对于它。
	Dir string
	// Key 是客户端凭据取值；空时取 DefaultKey。
	Key string
	// Extra 是额外注入的环境变量。
	Extra map[string]string
}

// CLI 跑一次子命令，返回合并输出与退出码；子命令根本没跑起来时退出码为 -1。
func (c *CheckRun) CLI(args ...string) (string, int) {
	key := c.Key
	if key == "" {
		key = DefaultKey
	}
	state, err := os.MkdirTemp("", "nova-example-check-")
	if err != nil {
		return err.Error(), -1
	}
	defer func() { _ = os.RemoveAll(state) }()

	env := map[string]string{
		"XDG_STATE_HOME":  filepath.Join(state, "state"),
		"XDG_CONFIG_HOME": filepath.Join(state, "config"),
		"NOVA_CLIENT_KEY": key,
		"NO_COLOR":        "1",
	}
	for name, value := range c.Extra {
		env[name] = value
	}

	cmd := exec.Command(c.Binary, args...)
	cmd.Dir = c.Dir
	cmd.Env = withEnv(os.Environ(), env)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return string(output), 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return string(output) + err.Error(), -1
	}
	return string(output), exitErr.ExitCode()
}

// LogBuffer 是进程输出的落点。
//
// 需要一把锁：写它的是 exec 包为进程 stdout/stderr 起的拷贝协程，读它的是调用方，
// 而调用方在进程还活着的时候就要读（就绪等待、失败信息）。裸 bytes.Buffer 在 -race 下会报竞争。
type LogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *LogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// withEnv 在环境变量表上覆盖若干项，先删掉同名项再追加。
//
// 直接追加会留下两份同名变量，而 execve 的接收方取哪一份属于未定义行为：
// 那种「本机恰好取到旧值」的差异正是最不该出现在示例里的东西。
//
// 取值为空串表示**不设**该变量，而不是设成空串：示例里要断言的正是
// 「变量缺失或为空都报错」，而一个空串与缺失在那种断言上没有区别，
// 因此这里不引入一个只能表达半件事的第三种状态。
func withEnv(base []string, extra map[string]string) []string {
	out := make([]string, 0, len(base)+len(extra))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := extra[name]; replaced {
			continue
		}
		out = append(out, entry)
	}
	for name, value := range extra {
		if value == "" {
			continue
		}
		out = append(out, name+"="+value)
	}
	return out
}
