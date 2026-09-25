# nova 的构建与本地门禁入口。
#
# 产物的两条出口是定死的：make build-binary 只写 bin/nova，make install 只写
# $(DESTDIR)$(BINDIR)/$(BIN)（缺省 /usr/local/bin/nova）。除此之外没有任何目标
# 会在仓库里生成文件，因此「编译文件散落在到处都是」没有发生的余地。

GO      ?= go
PREFIX  ?= /usr/local
BINDIR  ?= $(PREFIX)/bin
BIN     ?= nova
VERSION ?=

# 版本号的注入目标是包级变量 internal/version.Version。
# 放在 internal 而不是 main：version 子命令、--version、启动横幅三处都要读它，
# 且注入点可被单元测试替换，不必为验证优先级去改构建参数。
VERSION_PKG := github.com/sumwai/nova/internal/version

# make 会在执行 recipe 之前把 VERSION 展开一次再交给子进程，值里的 $(shell ...)
# 因而会在任何校验之前就被执行。这里用两步挡住那次展开：
#   - unexport 阻止 make 把展开后的值传给 recipe；
#   - $(value ...) 取回变量的原文（不再求值），由 recipe 的 shell 自行读取与校验。
unexport VERSION
export VERSION_RAW := $(value VERSION)

.PHONY: all build build-binary install uninstall test vet fmt fmt-check check release-dry-run clean help

all: build

## build: 全仓编译检查；只编译，不产出任何文件
build:
	$(GO) build ./...

## build-binary: 产出 bin/nova 并注入版本号
#
# 版本号取值顺序：VERSION 变量 > 最近的 git tag > 字面量 0.0.0。
#   - 用 --abbrev=0 只取「最近的 tag」，不用 git describe 的默认输出：默认输出形如
#     v0.0.0-2-g4700f09，它把「是哪个版本」与「距那个版本多远」挤进同一个字段，
#     结果是这个字段既不能当版本号去比较，也不是一个提交号。而「构建自哪个提交」
#     由二进制内嵌的 vcs.revision 单独回答，两件事不必合并。
#   - 不带 --always：没有任何 tag 时它失败并退出非零，于是退到 0.0.0。这正是我们要的——
#     版本号该是 tag，而不是一个伪装成版本号的提交哈希。
#   - --dirty 让工作区有未提交改动时带上 -dirty：那种构建不是任何一个 tag 的内容，
#     版本号里必须看得出来。
#   - 取到的值按白名单校验，含其它字符即构建失败。刻意不做静默清洗：清洗会产出一个
#     与 tag 对不上号的版本号，而那正是本目标要消灭的「不知道这是哪个版本」。
build-binary:
	@mkdir -p bin
	@set -eu; \
	version="$${VERSION_RAW:-$$(git describe --tags --dirty --abbrev=0 2>/dev/null || echo 0.0.0)}"; \
	case "$$version" in \
		*[!A-Za-z0-9._+-]*) \
			echo "错误：版本号 '$$version' 含非法字符，只允许字母、数字与 . _ + -" >&2; \
			exit 1 ;; \
	esac; \
	echo "构建 bin/$(BIN)（版本 $$version）"; \
	$(GO) build -ldflags "-X $(VERSION_PKG).Version=$$version" -o bin/$(BIN) ./cmd/nova

## install: 安装到 $(DESTDIR)$(BINDIR)/$(BIN)，缺省 /usr/local/bin/nova
install: build-binary
	@install -d "$(DESTDIR)$(BINDIR)" 2>/dev/null || { \
		echo "错误：无法创建目录 $(DESTDIR)$(BINDIR)" >&2; \
		echo "  提权安装：sudo make install" >&2; \
		echo "  或装到用户目录：PREFIX=\$$HOME/.local make install" >&2; \
		exit 1; }
	install -m 0755 bin/$(BIN) "$(DESTDIR)$(BINDIR)/$(BIN)"
	@echo "已安装 $(DESTDIR)$(BINDIR)/$(BIN)"

## uninstall: 删除已安装的可执行文件
uninstall:
	rm -f "$(DESTDIR)$(BINDIR)/$(BIN)"

## test: 单元测试，带竞态检测
test:
	$(GO) test -race ./...

## vet: go vet 静态检查
vet:
	$(GO) vet ./...

## fmt: 就地格式化
fmt:
	gofmt -l -w .

## fmt-check: 检查格式，有未格式化的文件即失败
fmt-check:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "以下文件未格式化（跑 make fmt 修）：" >&2; \
		echo "$$out" >&2; \
		exit 1; \
	fi

## check: 本地门禁 = build + vet + test + fmt-check
check: build vet test fmt-check

## release-dry-run: 预演一次发版，算出下一个版本号与发布说明，不改仓库、不打 tag
#
# 正式发版在合并到 main 时由 .github/workflows/release.yml 触发，两边调的是同一个
# scripts/release.sh。这个目标把同一段判定搬到手边，用来回答「这次合并会发成哪个版本」：
# 预演与真发用两份判定逻辑，是版本号规则最容易开始漂移的地方。
# 指定版本位：make release-dry-run BUMP=y
release-dry-run:
	@scripts/release.sh --dry-run $(if $(BUMP),--bump $(BUMP),)

## clean: 删除构建产物
clean:
	rm -rf bin/

## help: 列出全部目标
help:
	@echo "nova 可用目标："
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
