package limits

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// windowKind 是窗口的三种形态。
type windowKind int

const (
	// windowAbsolute 表示总量或余额：没有窗口边界，只能显式清除。
	windowAbsolute windowKind = iota
	// windowDuration 表示滚动窗口，写成数量加单位（1s/1m/5h/1d）。
	windowDuration
	// windowNatural 表示自然周期（day/week/month），按 tz 的日历边界切分。
	windowNatural
)

// windowSpec 是一条额度条目的窗口。
//
// 自然周期与滚动窗口的区别必须显式承载：自然周期按声明的时区切分，滚动窗口从本窗口
// 首次记账的时刻起算，absolute 没有边界。把它们压成一个 time.Duration 会让「一个月」
// 与「30 天」在跨月时给出不同的边界，而那正是自然周期存在的理由。
type windowSpec struct {
	raw       string
	kind      windowKind
	natural   string // day / week / month
	length    time.Duration
	loc       *time.Location
	anchor    time.Duration
	hasAnchor bool
}

// parseWindow 解析窗口写法。
//
// tz 为空时按 UTC 处理；自然周期缺 tz 的问题由 profile 层的校验挡在更前面，
// 这里只保证即使拿到缺省值也能算出一个确定的边界。
func parseWindow(raw, tz, anchor string) (windowSpec, error) {
	spec := windowSpec{raw: raw}
	switch raw {
	case "absolute":
		spec.kind = windowAbsolute
		return spec, nil
	case "day", "week", "month":
		spec.kind = windowNatural
		spec.natural = raw
	default:
		length, err := parseDurationWindow(raw)
		if err != nil {
			return windowSpec{}, err
		}
		spec.kind = windowDuration
		spec.length = length
	}

	loc, err := parseLocation(tz)
	if err != nil {
		return windowSpec{}, err
	}
	spec.loc = loc

	if anchor != "" {
		offset, err := time.ParseDuration(anchor)
		if err == nil {
			spec.anchor = offset
			spec.hasAnchor = true
		}
	}
	return spec, nil
}

// parseDurationWindow 把「数量 + 单位」解析成时长。单位 d 是 24 小时，不是自然日。
func parseDurationWindow(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, fmt.Errorf("窗口为空")
	}
	unit := raw[len(raw)-1]
	digits := raw[:len(raw)-1]
	count, err := strconv.Atoi(digits)
	if err != nil || count <= 0 {
		return 0, fmt.Errorf("窗口 %q 不是合法的滚动窗口", raw)
	}
	var scale time.Duration
	switch unit {
	case 's':
		scale = time.Second
	case 'm':
		scale = time.Minute
	case 'h':
		scale = time.Hour
	case 'd':
		scale = 24 * time.Hour
	default:
		return 0, fmt.Errorf("窗口 %q 的单位 %q 不被支持", raw, string(unit))
	}
	return time.Duration(count) * scale, nil
}

// parseLocation 解析时区，接受 IANA 名称（Asia/Shanghai）与纯偏移（+08:00、-0500、Z）。
//
// 纯偏移必须支持：额度文档由脚本或平台给出，它们常把时区写成 "+08:00" 这种字面偏移，
// 而不是依赖本机 tzdata 里存在同名地区。
func parseLocation(tz string) (*time.Location, error) {
	if tz == "" {
		return time.UTC, nil
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc, nil
	}
	for _, layout := range []string{"-07:00", "-0700", "-07"} {
		if parsed, err := time.Parse(layout, tz); err == nil {
			_, offset := parsed.Zone()
			return time.FixedZone(tz, offset), nil
		}
	}
	if tz == "Z" {
		return time.UTC, nil
	}
	return nil, fmt.Errorf("时区 %q 无法解析", tz)
}

// start 返回包含 t 的那个窗口的起点。
//
// 滚动窗口没有可对齐的日历边界，起点取传入时刻本身；调用方用窗口首次开始记账的时刻。
// 自然周期按 tz 的日历边界切分；声明了 anchor（可解析为时长）时把边界整体后移，
// 用于「脚本在每天的固定时刻下单」这类锚点。
func (w windowSpec) start(t time.Time) time.Time {
	if w.kind != windowNatural {
		return t
	}
	shifted := t
	if w.hasAnchor {
		shifted = t.Add(-w.anchor)
	}
	local := shifted.In(w.loc)
	var boundary time.Time
	switch w.natural {
	case "day":
		boundary = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, w.loc)
	case "week":
		// 周从周一开始：Go 的 Weekday 把周日算 0，需要先折算成「距周一的偏移」。
		offset := (int(local.Weekday()) + 6) % 7
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, w.loc)
		boundary = day.AddDate(0, 0, -offset)
	case "month":
		boundary = time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, w.loc)
	default:
		return t
	}
	if w.hasAnchor {
		return boundary.Add(w.anchor)
	}
	return boundary
}

// end 返回以 start 为起点的窗口的结束时刻。
func (w windowSpec) end(start time.Time) time.Time {
	switch w.kind {
	case windowAbsolute:
		return time.Time{}
	case windowNatural:
		switch w.natural {
		case "day":
			return start.AddDate(0, 0, 1)
		case "week":
			return start.AddDate(0, 0, 7)
		case "month":
			return start.AddDate(0, 1, 0)
		}
		return start
	default:
		return start.Add(w.length)
	}
}

// lengthFrom 返回以 start 为起点的窗口长度；absolute 没有长度，返回 0。
func (w windowSpec) lengthFrom(start time.Time) time.Duration {
	if w.kind == windowAbsolute {
		return 0
	}
	return w.end(start).Sub(start)
}

// String 返回窗口的书写形态，用于报错与 Reason 回显。
func (w windowSpec) String() string {
	if w.raw != "" {
		return w.raw
	}
	if w.kind == windowAbsolute {
		return "absolute"
	}
	return strings.TrimSpace(w.natural)
}
