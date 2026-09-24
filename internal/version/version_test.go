package version

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// withInjectedVersion 临时替换包级注入值，并在测试结束时还原。
//
// 不用 t.Setenv：Version 是构建期注入的变量，不是环境变量，
// 走环境变量会让这条测试验证的东西与生产路径不是一回事。
func withInjectedVersion(t *testing.T, value string) {
	t.Helper()
	previous := Version
	Version = value
	t.Cleanup(func() { Version = previous })
}

// buildInfoStub 造一个只回答指定 VCS 条目的构建信息读取函数。
func buildInfoStub(goVersion string, settings map[string]string) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		info := &debug.BuildInfo{GoVersion: goVersion}
		for key, value := range settings {
			info.Settings = append(info.Settings, debug.BuildSetting{Key: key, Value: value})
		}
		return info, true
	}
}

const fullRevision = "0123456789abcdef0123456789abcdef01234567"

func TestFromBuildInfoPrefersInjectedVersion(t *testing.T) {
	withInjectedVersion(t, "1.2.3")

	info := fromBuildInfo(buildInfoStub("go1.25.0", map[string]string{
		vcsRevisionKey: fullRevision,
	}))

	if info.Version != "1.2.3" {
		t.Errorf("版本号 = %q，期望注入值 %q", info.Version, "1.2.3")
	}
	// 注入值只覆盖版本号，提交身份仍要如实报出：这两条事实回答的是不同问题。
	if info.Revision != fullRevision {
		t.Errorf("提交 = %q，期望 %q", info.Revision, fullRevision)
	}
}

func TestFromBuildInfoFallsBackToShortRevision(t *testing.T) {
	withInjectedVersion(t, "")

	info := fromBuildInfo(buildInfoStub("go1.25.0", map[string]string{
		vcsRevisionKey: fullRevision,
	}))

	if want := fullRevision[:shortRevisionLength]; info.Version != want {
		t.Errorf("版本号 = %q，期望短提交 %q", info.Version, want)
	}
}

func TestFromBuildInfoFallsBackToLiteralWhenNothingIsKnown(t *testing.T) {
	withInjectedVersion(t, "")

	info := fromBuildInfo(func() (*debug.BuildInfo, bool) { return nil, false })

	if info.Version != fallbackVersion {
		t.Errorf("版本号 = %q，期望兜底值 %q", info.Version, fallbackVersion)
	}
	if info.GoVersion != runtime.Version() {
		t.Errorf("Go 版本 = %q，期望运行时版本 %q", info.GoVersion, runtime.Version())
	}
}

// 读不到构建信息时按「什么都没有」处理，不 panic。
//
// 这条分支的现实来源是「构建时不带 -buildvcs 且不在 git 仓库里」，
// 它必须退化成一份仍可读的输出，而不是让版本查询把进程搞崩。
func TestFromBuildInfoToleratesNilReader(t *testing.T) {
	withInjectedVersion(t, "")

	info := fromBuildInfo(nil)

	if info.Version != fallbackVersion {
		t.Errorf("版本号 = %q，期望 %q", info.Version, fallbackVersion)
	}
	if info.GoVersion == "" {
		t.Error("Go 版本为空；应退回运行时版本，否则版本输出首行会出现一个空槽")
	}
}

func TestFromBuildInfoTreatsEmptyGoVersionAsUnknown(t *testing.T) {
	withInjectedVersion(t, "")

	info := fromBuildInfo(buildInfoStub("", nil))

	if info.GoVersion != runtime.Version() {
		t.Errorf("Go 版本 = %q，期望退回运行时版本 %q", info.GoVersion, runtime.Version())
	}
}

// 脏标志只认小写 true：工具链写出的形态只有 true 与 false，
// 认下其它拼写等于给一个不存在的事实背书。
func TestFromBuildInfoOnlyAcceptsLowercaseTrueForModified(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"true", true},
		{"false", false},
		{"TRUE", false},
		{"True", false},
		{"1", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			withInjectedVersion(t, "")
			info := fromBuildInfo(buildInfoStub("go1.25.0", map[string]string{
				vcsModifiedKey: tt.value,
			}))
			if info.Modified != tt.want {
				t.Errorf("Modified = %v，期望 %v（原始取值 %q）", info.Modified, tt.want, tt.value)
			}
		})
	}
}

func TestShortRevision(t *testing.T) {
	tests := []struct {
		name     string
		revision string
		want     string
	}{
		{"完整哈希截到七位", fullRevision, fullRevision[:7]},
		{"恰好七位原样返回", "abcdefg", "abcdefg"},
		// 不补零：补零会造出一个仓库里根本不存在的提交号。
		{"不足七位原样返回", "abc", "abc"},
		{"空串原样返回", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShortRevision(tt.revision); got != tt.want {
				t.Errorf("ShortRevision(%q) = %q，期望 %q", tt.revision, got, tt.want)
			}
		})
	}
}
