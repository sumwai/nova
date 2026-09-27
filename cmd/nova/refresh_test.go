package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/nova/internal/config"
)

// TestRefreshProfileSourcesSkipsNonRemote 守护自动刷新只处理远端源。
//
// builtin 随二进制发布、本地源就在磁盘上，两者都没有可拉取的东西；把它们也走一遍
// 同步只会白白读磁盘。
func TestRefreshProfileSourcesSkipsNonRemote(t *testing.T) {
	cfg := &config.Config{Profiles: config.DefaultProfiles()}
	var out bytes.Buffer

	changed, err := refreshProfileSources(context.Background(), cfg, t.TempDir(), &out)
	if err != nil || changed {
		t.Fatalf("builtin 不应触发刷新：changed=%v err=%v out=%q", changed, err, out.String())
	}
}

// TestRefreshProfileSourcesReportsRemoteFailure 守护远端不可达时报失败且不报「有更新」。
func TestRefreshProfileSourcesReportsRemoteFailure(t *testing.T) {
	remote := unreachableRemote(t)
	cfg := &config.Config{Profiles: config.Profiles{
		Sources: []config.ProfileSource{{Kind: config.SourceRemote, URL: remote}},
		Refresh: time.Hour,
	}}
	var out bytes.Buffer

	changed, err := refreshProfileSources(context.Background(), cfg, t.TempDir(), &out)
	if err == nil {
		t.Fatal("远端不可达应返回错误")
	}
	if changed {
		t.Error("失败不应报告为有更新")
	}
	if !strings.Contains(out.String(), "失败 "+remote) {
		t.Errorf("输出 = %q，期望含失败行", out.String())
	}
}
