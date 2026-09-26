package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// profilePubKey 造一段指定字节数的 base64 公钥取值，供 profiles 用例使用。
func profilePubKey(size int) string {
	return base64.StdEncoding.EncodeToString(make([]byte, size))
}

// TestParseProfilesBlock 覆盖合法块：三种源、多条 key、自定义 refresh。
func TestParseProfilesBlock(t *testing.T) {
	main := profilePubKey(ed25519.PublicKeySize)
	next := profilePubKey(ed25519.PublicKeySize)
	cfg := mustParse(t, "version 1\n"+
		"profiles {\n"+
		"    source builtin\n"+
		"    source https://example.com/nova/profiles key main "+main+" key next "+next+"\n"+
		"    source ./profiles.local\n"+
		"    refresh 6h\n"+
		"}\n")

	if cfg.Profiles.Refresh != 6*time.Hour {
		t.Errorf("刷新周期 = %v，期望 6h", cfg.Profiles.Refresh)
	}
	if len(cfg.Profiles.Sources) != 3 {
		t.Fatalf("源数 = %d，期望 3", len(cfg.Profiles.Sources))
	}

	if got := cfg.Profiles.Sources[0]; got.Kind != SourceBuiltin || got.URL != "" || got.Path != "" {
		t.Errorf("第一个源 = %+v，期望只有 builtin", got)
	}

	remote := cfg.Profiles.Sources[1]
	if remote.Kind != SourceRemote {
		t.Errorf("第二个源的种类 = %q，期望 %q", remote.Kind, SourceRemote)
	}
	if remote.URL != "https://example.com/nova/profiles" {
		t.Errorf("远端地址 = %q", remote.URL)
	}
	if len(remote.Keys) != 2 {
		t.Fatalf("公钥数 = %d，期望 2", len(remote.Keys))
	}
	if remote.Keys[0].Name != "main" || remote.Keys[1].Name != "next" {
		t.Errorf("公钥名 = %q / %q，期望 main / next", remote.Keys[0].Name, remote.Keys[1].Name)
	}
	if len(remote.Keys[0].PublicKey) != ed25519.PublicKeySize {
		t.Errorf("公钥长度 = %d，期望 %d", len(remote.Keys[0].PublicKey), ed25519.PublicKeySize)
	}

	local := cfg.Profiles.Sources[2]
	if local.Kind != SourceLocal {
		t.Errorf("第三个源的种类 = %q，期望 %q", local.Kind, SourceLocal)
	}
	if local.Path != "profiles.local" {
		t.Errorf("本地路径 = %q，期望相对路径 profiles.local", local.Path)
	}
}

// 整块省略时等价于只有 source builtin 且刷新 24h。
func TestProfilesDefaultWhenOmitted(t *testing.T) {
	cfg := mustParse(t, "version 1\n")

	if len(cfg.Profiles.Sources) != 1 {
		t.Fatalf("源数 = %d，期望 1", len(cfg.Profiles.Sources))
	}
	if got := cfg.Profiles.Sources[0]; got.Kind != SourceBuiltin {
		t.Errorf("缺省源的种类 = %q，期望 %q", got.Kind, SourceBuiltin)
	}
	if cfg.Profiles.Refresh != 24*time.Hour {
		t.Errorf("缺省刷新周期 = %v，期望 24h", cfg.Profiles.Refresh)
	}
}

// 本地相对路径相对于写下这一行的文件解析：这里的 source 行来自被 import 的子目录文件。
func TestProfileLocalPathResolvesRelativeToWritingFile(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatalf("建子目录失败：%v", err)
	}
	main := filepath.Join(dir, "Novafile")
	writeFile(t, main, "version 1\nimport sub/extra\n")
	writeFile(t, filepath.Join(sub, "extra"), "profiles {\n    source ./profiles.local\n}\n")

	cfg, err := Load(main)
	if err != nil {
		t.Fatalf("Load 意外失败：%v", err)
	}
	if len(cfg.Profiles.Sources) != 1 {
		t.Fatalf("源数 = %d，期望 1", len(cfg.Profiles.Sources))
	}
	src := cfg.Profiles.Sources[0]
	if src.Kind != SourceLocal {
		t.Fatalf("源种类 = %q，期望 %q", src.Kind, SourceLocal)
	}
	want := filepath.Join(sub, "profiles.local")
	if src.Path != want {
		t.Errorf("本地路径 = %q，期望 %q", src.Path, want)
	}
}

// 非法场景必须带文件路径与行号，不能只报「出错了」。
func TestProfilesErrors(t *testing.T) {
	pub := profilePubKey(ed25519.PublicKeySize)
	short := profilePubKey(16)
	tests := []struct {
		name string
		src  string
		line int
		want []string
	}{
		{
			name: "http 地址被拒",
			src: "version 1\n" +
				"profiles {\n" +
				"    source http://example.com/nova/profiles\n" +
				"}\n",
			line: 3,
			want: []string{"只支持 https", "http://example.com/nova/profiles"},
		},
		{
			name: "base64 非法",
			src: "version 1\n" +
				"profiles {\n" +
				"    source https://example.com/nova/profiles key main !!!不是base64!!!\n" +
				"}\n",
			line: 3,
			want: []string{"不是合法的 base64"},
		},
		{
			name: "公钥长度不对",
			src: "version 1\n" +
				"profiles {\n" +
				"    source https://example.com/nova/profiles key main " + short + "\n" +
				"}\n",
			line: 3,
			want: []string{"必须是 32 字节"},
		},
		{
			name: "同 source 内 key 名重复",
			src: "version 1\n" +
				"profiles {\n" +
				"    source https://example.com/nova/profiles key main " + pub + " key main " + pub + "\n" +
				"}\n",
			line: 3,
			want: []string{"不能重复"},
		},
		{
			name: "重复 builtin",
			src: "version 1\n" +
				"profiles {\n" +
				"    source builtin\n" +
				"    source builtin\n" +
				"}\n",
			line: 4,
			want: []string{"声明过", "第 3 行"},
		},
		{
			name: "重复远端地址",
			src: "version 1\n" +
				"profiles {\n" +
				"    source https://example.com/nova/profiles\n" +
				"    source https://example.com/nova/profiles\n" +
				"}\n",
			line: 4,
			want: []string{"声明过", "第 3 行"},
		},
		{
			name: "大小写不同的同一远端源",
			src: "version 1\n" +
				"profiles {\n" +
				"    source https://example.com/nova/profiles\n" +
				"    source https://EXAMPLE.com/nova/profiles\n" +
				"}\n",
			line: 4,
			want: []string{"声明过", "第 3 行"},
		},
		{
			name: "显式默认端口与缺省端口是同一个源",
			src: "version 1\n" +
				"profiles {\n" +
				"    source https://example.com/nova/profiles\n" +
				"    source https://example.com:443/nova/profiles\n" +
				"}\n",
			line: 4,
			want: []string{"声明过"},
		},
		{
			name: "大写 http 也按明文 http 拒绝",
			src: "version 1\n" +
				"profiles {\n" +
				"    source HTTP://example.com/nova/profiles\n" +
				"}\n",
			line: 3,
			want: []string{"只支持 https"},
		},
		{
			name: "refresh 非正",
			src: "version 1\n" +
				"profiles {\n" +
				"    refresh 0s\n" +
				"}\n",
			line: 3,
			want: []string{"不是正的时长"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cerr := parseErr(t, tt.src)
			if cerr.File != "Novafile" {
				t.Errorf("文件 = %q，期望 Novafile", cerr.File)
			}
			if cerr.Line != tt.line {
				t.Errorf("行号 = %d，期望 %d（消息 %q）", cerr.Line, tt.line, cerr.Msg)
			}
			for _, want := range tt.want {
				if !strings.Contains(cerr.Msg, want) {
					t.Errorf("消息 = %q，期望包含 %q", cerr.Msg, want)
				}
			}
			if !strings.Contains(cerr.Error(), "Novafile:") {
				t.Errorf("排版 = %q，期望带文件与行号前缀", cerr.Error())
			}
		})
	}
}

// profiles / source / refresh 都在指令清单里：二进制自己回答「认哪些写法」。
func TestProfilesListedAsInstructions(t *testing.T) {
	names := InstructionNames()
	for _, want := range []string{directiveProfiles, directiveSource, directiveRefresh} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("指令清单里没有 %q：%v", want, names)
		}
	}
}
