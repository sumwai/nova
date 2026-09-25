#!/bin/sh
# nova 的发版判定。
#
# 职责边界：本脚本只回答两件事——「下一个版本号是多少」与「这个版本的内容是什么」，
# 具体是改写 CHANGELOG 的段落标题、提交、打 tag、抽出发布说明。编译产物与 GitHub
# Release 由调用方（.github/workflows/release.yml）负责。判定与产物分开，是为了让本地
# 预演（make release-dry-run）与 CI 正式发布走同一段代码：版本号规则在两地各写一份，
# 迟早会在某一处先漂移。
#
# 版本号规则（README「配置语法代数」一节）：兼容变更 z+1；不兼容变更 y+1 且 z=0；
# 大功能变更 x+1、y=0、z=0。x 位只由 --bump x 显式指定，不从提交推断：
# 「大功能」是人的判断，不是能从提交信息里读出来的事实。
#
# 用法：
#   scripts/release.sh [--bump auto|z|y|x] [--dry-run] [--no-push] [--notes FILE] [--branch main]
#       从 ## Unreleased 段推出下一个版本，改写标题、提交、打 tag、推送。
#   scripts/release.sh --tag vX.Y.Z [--notes FILE]
#       只校验该 tag 在 CHANGELOG 里有对应段落并抽出发布说明，不写仓库。

set -eu

CHANGELOG=CHANGELOG.md
SCHEMA_FILE=internal/config/config.go
SCHEMA_CONST=CurrentSchema
BRANCH=${RELEASE_BRANCH:-main}

bump=auto
dry_run=0
push=1
notes_file=
tag_arg=

say() { printf '%s\n' "$1"; }
die() { printf '错误：%s\n' "$1" >&2; exit 1; }

usage() {
	printf '%s\n' \
		'用法：' \
		'  scripts/release.sh [--bump auto|z|y|x] [--dry-run] [--no-push] [--notes FILE] [--branch main]' \
		'      从 CHANGELOG 的 ## Unreleased 段推出下一个版本并落地。' \
		'  scripts/release.sh --tag vX.Y.Z [--notes FILE]' \
		'      只校验该 tag 在 CHANGELOG 里有段落并抽出发布说明，不写仓库。'
}

# emit 把结果写给 CI；GITHUB_OUTPUT 未设时（本地跑）丢弃。两条取值都不含空白，
# 因此不必按 GITHUB_OUTPUT 的多行格式编码。
emit() {
	{
		printf 'released=%s\n' "$1"
		printf 'tag=%s\n' "$2"
	} >> "${GITHUB_OUTPUT:-/dev/null}"
}

# has_content 判断一段文本里有没有实质内容。空行不算内容：
# ## Unreleased 段只剩下换行时不该发版。
has_content() { printf '%s\n' "$1" | grep -qE '[^[:space:]]'; }

# section_body 抽出 "## <标题>" 到下一个二级标题之间的正文，标题不存在时返回非零。
# 标题按整行精确比对，不做前缀匹配：`## v0.1.1-rc1` 与 `## v0.1.1` 是两段，
# 前缀匹配会让它们互相顶掉。
section_body() {
	awk -v want="## $1" '
		/^##[ \t]/ {
			if (inside) exit
			if ($0 == want) { inside = 1; found = 1; next }
			next
		}
		inside { print }
		END { if (!found) exit 1 }
	' "$CHANGELOG"
}

# release_notes 是发布说明：段正文去掉开头空行后的原样内容。
# 不重排版：CHANGELOG 的段落本来就是给人读的，再拼一次会把两处文案变成两份。
release_notes() {
	body=$(section_body "$1") || return 1
	printf '%s\n' "$body" | awk 'NF || started { started = 1; print }'
}

# schema_of_file 读一条 `const <名> = <值>` 声明的取值，读不到即返回非零。
# 常量名或写法变了，配置代数判定就失效，静默当成「没变」会漏掉一次 y 位。
schema_of_file() {
	val=$(awk -v name="$SCHEMA_CONST" '$1 == "const" && $2 == name && $3 == "=" { print $4; exit }' "$1")
	[ -n "$val" ] || return 1
	printf '%s\n' "$val"
}

# schema_at_tag 读某个 tag 上的代数取值。该 tag 上还没有这个声明（代数本身是后加的）
# 或读不出来时返回非零，调用方按「已变」处理。
schema_at_tag() {
	git show "$1:$SCHEMA_FILE" > "$tmp/schema.go" 2>/dev/null || return 1
	schema_of_file "$tmp/schema.go"
}

# breaking_since 判断某个 tag 之后有没有破坏性变更。
#
# 读提交题注与正文，不读 PR 正文：仓库用 squash 合并，进入历史的是分支上的提交信息，
# PR 正文不在其中——照 PR 正文判定会永远判不出破坏性变更。
breaking_since() {
	if git log --format='%s' "$1..HEAD" | grep -qE '^[A-Za-z]+(\([^)]*\))?!:'; then
		return 0
	fi
	git log --format='%b' "$1..HEAD" | grep -q 'BREAKING CHANGE'
}

# parse_version 把 vX.Y.Z 拆成三段。别的形态不猜：基数错了，推出来的下一个版本号也是错的。
parse_version() {
	rest=${1#v}
	case "$rest" in *.*.*) ;; *) die "$1 不是 vX.Y.Z 形式，无法推出下一个版本号" ;; esac
	major=${rest%%.*}
	rest=${rest#*.}
	minor=${rest%%.*}
	patch=${rest#*.}
	case "$patch" in *.*) die "$1 不是 vX.Y.Z 形式，无法推出下一个版本号" ;; esac
	case "$major" in ''|*[!0-9]*) die "$1 的版本段不是数字" ;; esac
	case "$minor" in ''|*[!0-9]*) die "$1 的版本段不是数字" ;; esac
	case "$patch" in ''|*[!0-9]*) die "$1 的版本段不是数字" ;; esac
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--bump) shift; [ "$#" -gt 0 ] || die "--bump 缺少取值"; bump=$1 ;;
		--bump=*) bump=${1#*=} ;;
		--tag) shift; [ "$#" -gt 0 ] || die "--tag 缺少取值"; tag_arg=$1 ;;
		--tag=*) tag_arg=${1#*=} ;;
		--notes) shift; [ "$#" -gt 0 ] || die "--notes 缺少取值"; notes_file=$1 ;;
		--notes=*) notes_file=${1#*=} ;;
		--branch) shift; [ "$#" -gt 0 ] || die "--branch 缺少取值"; BRANCH=$1 ;;
		--branch=*) BRANCH=${1#*=} ;;
		--dry-run) dry_run=1 ;;
		--no-push) push=0 ;;
		-h|--help) usage; exit 0 ;;
		*) die "未知参数 $1（--help 看用法）" ;;
	esac
	shift
done

case "$bump" in
	auto|z|y|x) ;;
	patch) bump=z ;;
	minor) bump=y ;;
	major) bump=x ;;
	*) die "--bump 只接受 auto、z、y、x" ;;
esac
explicit=0
[ "$bump" = auto ] || explicit=1

git rev-parse --show-toplevel >/dev/null 2>&1 || die "不在 git 仓库内"
cd "$(git rev-parse --show-toplevel)"
[ -f "$CHANGELOG" ] || die "找不到 $CHANGELOG"

tmp=$(mktemp -d) || die "无法创建临时目录"
trap 'rm -rf "$tmp"' EXIT INT TERM HUP

# --tag 形态：为已存在的 tag 抽发布说明。它不改仓库，因此不需要干净的工作区，
# 也不参与版本号判定——tag 是谁打的、怎么算出来的，都不是这一段要回答的事。
if [ -n "$tag_arg" ]; then
	parse_version "$tag_arg"
	if ! notes=$(release_notes "$tag_arg"); then
		die "$CHANGELOG 里没有 ## $tag_arg 段，无法为这个 tag 生成发布说明"
	fi
	has_content "$notes" || die "$CHANGELOG 的 ## $tag_arg 段没有内容"
	if [ -n "$notes_file" ]; then
		printf '%s\n' "$notes" > "$notes_file"
		say "发布说明已写到 $notes_file"
	fi
	emit true "$tag_arg"
	exit 0
fi

# 工作区必须干净：本形态会 commit，把别人的在途改动一起提交进去就不可挽回了。
if [ "$dry_run" != 1 ] && [ -n "$(git status --porcelain)" ]; then
	die "工作区有未提交改动；本次发版会 commit，先在干净的工作区上跑"
fi

# 取最近的版本 tag。--match 排掉非版本 tag；--abbrev=0 只取 tag 本身，
# 不用 git describe 的 -N-g<hash> 形态——那串东西既不是版本号也不是提交号。
# 一个版本 tag 都还没有时基数取 v0.0.0，并把「没有基线」记成事实：
# 拿一个不存在的 tag 去跑区间查询，git log 会直接报 fatal，判定随之失去依据。
if last=$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null); then
	baseline=1
else
	last=v0.0.0
	baseline=0
	say "提示：仓库里还没有版本 tag，基数按 $last 计"
fi

if ! body=$(section_body Unreleased); then
	say "$CHANGELOG 没有 ## Unreleased 段，本次不发版"
	emit false ""
	exit 0
fi
if ! has_content "$body"; then
	say "## Unreleased 段没有内容，本次不发版"
	emit false ""
	exit 0
fi

parse_version "$last"

schema_now=$(schema_of_file "$SCHEMA_FILE") || die "$SCHEMA_FILE 里读不到 $SCHEMA_CONST 的取值，配置代数判定失效"
schema_changed=0
if [ "$baseline" = 1 ]; then
	if schema_prev=$(schema_at_tag "$last"); then
		[ "$schema_prev" = "$schema_now" ] || schema_changed=1
	else
		say "提示：$last 上读不到 $SCHEMA_CONST，该声明可能是后加的，按代数已变处理"
		schema_changed=1
	fi
fi

if [ "$bump" = auto ]; then
	if [ "$baseline" = 0 ]; then
		bump=z
		why="仓库里还没有版本 tag，按首个版本处理"
	elif breaking_since "$last"; then
		bump=y
		why="自 $last 起有破坏性变更（题注带 ! 或正文带 BREAKING CHANGE）"
	else
		bump=z
		why="自 $last 起没有破坏性变更"
	fi
else
	why="由 --bump $bump 指定"
fi

# 代数变了必然是不兼容变更。这条守的是反方向：判定成 z 位的发版直接中断，
# 而不是发出一个与「不兼容变更 y+1」规则不符的版本号。
if [ "$schema_changed" = 1 ] && [ "$bump" = z ]; then
	if [ "$explicit" = 1 ]; then
		die "配置语法代数已从 ${schema_prev:-$last 上的取值} 变为 $schema_now；不兼容的语法变更要求升 y 位，与 --bump z 冲突"
	fi
	bump=y
	why="配置语法代数已从 ${schema_prev:-未知} 变为 $schema_now"
fi
if [ "$bump" = y ] && [ "$schema_changed" = 0 ]; then
	say "提示：判定升 y 位，但配置语法代数仍为 $schema_now；若本次含不兼容的配置语法变更，规则要求同时把 $SCHEMA_CONST +1。"
fi

case "$bump" in
	x) major=$((major + 1)); minor=0; patch=0 ;;
	y) minor=$((minor + 1)); patch=0 ;;
	z) patch=$((patch + 1)) ;;
esac
next="v${major}.${minor}.${patch}"

if git rev-parse -q --verify "refs/tags/$next" >/dev/null; then
	die "tag $next 已存在；先确认上个 tag 是否漏打，或改用 --bump 指定更高的版本位"
fi

if [ "$dry_run" = 1 ]; then
	say "上个 tag：$last"
	say "版本位：$bump（$why）"
	say "下一个版本：$next"
	say "发布说明："
	printf '%s\n' "$body"
	say "（预演：仓库未改动、tag 未创建）"
	exit 0
fi

# 只替换第一个 ## Unreleased 行，其余内容逐字保留：段落正文是人工写的事实，
# 任何重排都会让 diff 里混进与本次发布无关的改动。
awk -v heading="## $next" '
	!renamed && /^##[ \t]+Unreleased[ \t]*$/ { print heading; renamed = 1; next }
	{ print }
' "$CHANGELOG" > "$tmp/CHANGELOG"
mv "$tmp/CHANGELOG" "$CHANGELOG"

notes=$(release_notes "$next") || die "改写后读不到 ## $next 段，CHANGELOG 结构不符合预期"
has_content "$notes" || die "改写后 ## $next 段没有内容"
if [ -n "$notes_file" ]; then
	printf '%s\n' "$notes" > "$notes_file"
	say "发布说明已写到 $notes_file"
fi

git add "$CHANGELOG"
git commit -q -m "docs: 发布 $next"
git tag "$next"
if [ "$push" = 1 ]; then
	git push origin "HEAD:refs/heads/$BRANCH"
	git push origin "refs/tags/$next"
else
	say "已跳过推送（--no-push）：提交与 tag 只留在本地"
fi

say "已发布 $next（$bump 位：$why）"
emit true "$next"
