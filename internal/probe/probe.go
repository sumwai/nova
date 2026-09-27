// Package probe 运行档案声明的外部用量探测。
//
// 边界按设计第五节：exec 只允许落在 ${XDG_DATA_HOME}/nova/probes/ 下的绝对路径，
// 禁止 PATH 查找；命令以 argv 数组直接 execve，不经 shell；环境变量清空、工作目录
// 固定，超时后强杀。本包不做自动调度，由调用方在显式命令（nova limits check）里触发。
//
// 仍待落地的约束见设计第五节：首次启用某档案命令的人工确认与「档案 id ↔ 命令」
// 绑定持久化、本地源目录属主与权限判定、远端源不得引用 exec。这些都需要调用方
// 提供源种类与交互，不在本包内。
package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sumwai/nova/internal/profile"
)

// DefaultTimeout 是一次 exec 探测的硬超时。
const DefaultTimeout = 30 * time.Second

// Runner 执行 exec 探测。
type Runner struct {
	// DataDir 是 nova 的数据目录（${XDG_DATA_HOME}/nova）；probes 子目录是唯一
	// 允许存放探测命令的落点。
	DataDir string

	// Timeout 是单次探测的硬超时；<= 0 时用 DefaultTimeout。
	Timeout time.Duration
}

// ProbesDir 返回允许存放探测命令的目录。
func (r *Runner) ProbesDir() string {
	return filepath.Join(r.DataDir, "probes")
}

// Run 执行一份 exec 探测，返回解析后的额度文档。
//
// 输出按 profile.ParseLimitsYAML 解析，因此与档案加载共用同一套形状校验与语义校验；
// 探测脚本不必自己保证 schema，写错会在 stderr 的报错里指出具体字段。
func (r *Runner) Run(ctx context.Context, execPath string) (*profile.LimitsDoc, error) {
	path, err := r.resolve(execPath)
	if err != nil {
		return nil, err
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// argv 只有一个元素：档案给的是可执行文件路径，没有位置参数的位置。
	// 直接 execve 不经 shell，路径里的元字符因此不会被解释。
	cmd := osexec.CommandContext(runCtx, path)
	cmd.Env = []string{}    // 清空环境：探测命令不得读走 nova 进程的环境
	cmd.Dir = r.ProbesDir() // 固定工作目录
	// 命令可能 fork 子进程（如 shell 调外部命令），子进程会继承 stdout 管道；
	// 只杀直接子进程时 Wait 会一直等到管道关闭。WaitDelay 让 Wait 在取消后
	// 有界地返回，而不是被一个不听话的孙进程拖住。
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if runCtx.Err() != nil {
			return nil, fmt.Errorf("探测 %s 超时（%s）后被终止", path, timeout)
		}
		var exitErr *osexec.ExitError
		if errors.As(err, &exitErr) {
			if detail := strings.TrimSpace(stderr.String()); detail != "" {
				return nil, fmt.Errorf("探测 %s 退出码 %d：%s", path, exitErr.ExitCode(), detail)
			}
			return nil, fmt.Errorf("探测 %s 退出码 %d", path, exitErr.ExitCode())
		}
		return nil, fmt.Errorf("启动探测 %s 失败：%v", path, err)
	}

	doc, err := profile.ParseLimitsYAML(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("探测 %s 的输出不是合法额度文档：%v", path, err)
	}
	return doc, nil
}

// resolve 校验 exec 路径并返回清理后的绝对路径。
//
// 三类拒绝各有独立措辞：相对路径说明「禁止 PATH 查找」；越出 probes 目录说明唯一
// 允许的落点；组或其他人可写说明「任何人可改写它，不能当可信可执行体」。
func (r *Runner) resolve(execPath string) (string, error) {
	if !filepath.IsAbs(execPath) {
		return "", fmt.Errorf("探测命令 %q 不是绝对路径；exec 禁止 PATH 查找", execPath)
	}
	dir := r.ProbesDir()
	clean := filepath.Clean(execPath)
	rel, err := filepath.Rel(dir, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("探测命令 %s 不在 %s 以内；exec 只允许放在这个目录下", clean, dir)
	}
	info, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("读取探测命令 %s 失败：%v", clean, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("探测命令 %s 是目录", clean)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("探测命令 %s 对组或其他人可写（%s）；先去掉写权限", clean, info.Mode().Perm())
	}
	return clean, nil
}
