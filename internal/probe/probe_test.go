package probe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeProbe 在数据目录的 probes 子目录下放一个可执行脚本，返回它的绝对路径。
func writeProbe(t *testing.T, dataDir, name, script string) string {
	t.Helper()
	dir := filepath.Join(dataDir, "nova", "probes")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("创建 probes 目录失败：%v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("写入探测脚本失败：%v", err)
	}
	return path
}

const quotaScript = `#!/bin/sh
printf '%s\n' \
  'schema: 1' \
  'source: exec' \
  'observed_at: 2026-09-27T00:00:00Z' \
  'account:' \
  '  - { kind: quota, metric: usd, window: 5h, limit: 14, remaining: 5.2 }'
`

func TestRunParsesLimitsDoc(t *testing.T) {
	dataDir := t.TempDir()
	path := writeProbe(t, dataDir, "usage.sh", quotaScript)

	doc, err := (&Runner{DataDir: filepath.Join(dataDir, "nova")}).Run(context.Background(), path)
	if err != nil {
		t.Fatalf("探测应成功：%v", err)
	}
	if doc.Source != "exec" || doc.ObservedAt != "2026-09-27T00:00:00Z" {
		t.Fatalf("文档头部 = %+v", doc)
	}
	if len(doc.Account) != 1 || doc.Account[0].Remaining == nil || *doc.Account[0].Remaining != 5.2 {
		t.Fatalf("账号级条目 = %+v", doc.Account)
	}
}

func TestRunRejectsRelativePath(t *testing.T) {
	_, err := (&Runner{DataDir: filepath.Join(t.TempDir(), "nova")}).Run(context.Background(), "usage.sh")
	if err == nil || !strings.Contains(err.Error(), "绝对路径") {
		t.Fatalf("相对路径应被拒，实际 %v", err)
	}
}

func TestRunRejectsOutsideProbesDir(t *testing.T) {
	dataDir := t.TempDir()
	outside := filepath.Join(dataDir, "elsewhere.sh")
	if err := os.WriteFile(outside, []byte(quotaScript), 0o700); err != nil {
		t.Fatalf("写入脚本失败：%v", err)
	}
	_, err := (&Runner{DataDir: filepath.Join(dataDir, "nova")}).Run(context.Background(), outside)
	if err == nil || !strings.Contains(err.Error(), "不在") {
		t.Fatalf("越界路径应被拒，实际 %v", err)
	}
}

func TestRunRejectsWritableByOthers(t *testing.T) {
	dataDir := t.TempDir()
	path := writeProbe(t, dataDir, "usage.sh", quotaScript)
	if err := os.Chmod(path, 0o777); err != nil {
		t.Fatalf("改权限失败：%v", err)
	}
	_, err := (&Runner{DataDir: filepath.Join(dataDir, "nova")}).Run(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "可写") {
		t.Fatalf("组/其他人可写的命令应被拒，实际 %v", err)
	}
}

func TestRunReportsNonZeroExit(t *testing.T) {
	dataDir := t.TempDir()
	path := writeProbe(t, dataDir, "usage.sh", "#!/bin/sh\necho '上游不可达' >&2\nexit 3\n")

	_, err := (&Runner{DataDir: filepath.Join(dataDir, "nova")}).Run(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "退出码 3") || !strings.Contains(err.Error(), "上游不可达") {
		t.Fatalf("非零退出应带码与 stderr，实际 %v", err)
	}
}

func TestRunTimesOut(t *testing.T) {
	dataDir := t.TempDir()
	path := writeProbe(t, dataDir, "usage.sh", "#!/bin/sh\n/bin/sleep 5\n")

	start := time.Now()
	_, err := (&Runner{DataDir: filepath.Join(dataDir, "nova"), Timeout: 100 * time.Millisecond}).
		Run(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("超时应被报告，实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("超时后应立即终止，实际耗时 %v", elapsed)
	}
}

func TestRunRejectsInvalidOutput(t *testing.T) {
	dataDir := t.TempDir()
	path := writeProbe(t, dataDir, "usage.sh", "#!/bin/sh\necho '不是额度文档'\n")

	_, err := (&Runner{DataDir: filepath.Join(dataDir, "nova")}).Run(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "额度文档") {
		t.Fatalf("非法输出应被拒，实际 %v", err)
	}
}

// TestRunClearsEnvironment 守护探测进程拿不到 nova 的环境变量。
func TestRunClearsEnvironment(t *testing.T) {
	dataDir := t.TempDir()
	path := writeProbe(t, dataDir, "usage.sh", `#!/bin/sh
if [ -n "$NOVA_PROBE_SECRET" ]; then
  echo "环境变量泄漏：$NOVA_PROBE_SECRET" >&2
  exit 1
fi
printf '%s\n' 'schema: 1' 'source: exec' 'observed_at: 2026-09-27T00:00:00Z' 'account:' '  - { kind: quota, metric: usd, window: 5h, limit: 1, remaining: 1 }'
`)
	t.Setenv("NOVA_PROBE_SECRET", "leaked")
	if _, err := (&Runner{DataDir: filepath.Join(dataDir, "nova")}).Run(context.Background(), path); err != nil {
		t.Fatalf("探测进程不应看到 nova 的环境变量：%v", err)
	}
}
