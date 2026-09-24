// Package sse 提供不含协议语义的 Server-Sent Events（服务器发送事件，SSE）帧读取器。
//
// 读取器从 io.Reader 逐帧读取，每读满一帧立即返回该帧的事件名、data 正文与原始字节，
// 不在内部缓存多帧；原始字节供响应侧按原报文透传使用。本包只依赖标准库，
// 不导入任何协议适配器，也不认识具体协议的事件名。
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

const (
	// initialBufferBytes 是底层读取缓冲的初始字节数。它只决定起始内存占用；
	// 单行更长时读取器按片段累积，直到 maxFrameBytes。
	initialBufferBytes = 64 * 1024
	// maxFrameBytes 是单帧允许的最大字节数（含字段行、注释行与结尾空行）。
	// 超过即返回 ErrFrameTooLarge，避免异常上游用超长帧撑爆内存。
	maxFrameBytes = 1024 * 1024
)

// 本包只识别的两个 SSE 字段名。
const (
	eventField = "event"
	dataField  = "data"
)

// ErrFrameTooLarge 表示单帧字节数超过 maxFrameBytes。
var ErrFrameTooLarge = errors.New("sse: 单帧字节数超过上限")

// Frame 是一条完整的 SSE 帧。
type Frame struct {
	// Event 是 event 字段的值；该帧未给出 event 字段时为空串。
	Event string
	// Data 是该帧全部 data 字段按换行连接后的正文。
	// 不含 data 字段的帧（心跳、注释、只有 event 字段）不产生 Frame。
	Data []byte
	// Raw 是该帧收到的原始字节，含字段行、注释行与结尾空行，可直接作为原报文透传。
	Raw []byte
}

// Reader 从 io.Reader 逐帧读取 SSE。Reader 不是并发安全的。
type Reader struct {
	br         *bufio.Reader
	onActivity func()
}

// NewReader 构造逐帧读取器，不报告底层字节到达事件。
func NewReader(r io.Reader) *Reader {
	return NewReaderWithActivity(r, nil)
}

// NewReaderWithActivity 构造逐帧读取器，并在每次从底层读到字节后同步调用 onActivity。
//
// 回调用于按「底层有字节到达即视为有进展」刷新空闲超时看门狗：注释行与纯空块（心跳）
// 不产生帧，但字节确实到达过，只发心跳的长思考流不应被看门狗切断。
// onActivity 为 nil 时等价于 NewReader；回调在调用 Next 的同一协程内、读到字节后触发。
func NewReaderWithActivity(r io.Reader, onActivity func()) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, initialBufferBytes), onActivity: onActivity}
}

// Next 返回下一条帧。
//
// 空行结束一帧；流在结尾没有空行时也会收尾。LF 与 CRLF 换行均可识别。
// 行首注释（`:`）与不含 data 字段的空块（心跳）不产生帧，但同样触发活动回调。
// 整条流读完时返回 io.EOF；单帧超过 maxFrameBytes 时返回 ErrFrameTooLarge。
func (r *Reader) Next() (Frame, error) {
	var (
		event string
		data  []byte
		raw   []byte
	)
	for {
		rawLine, err := r.readLine(maxFrameBytes - len(raw))
		if err != nil && !errors.Is(err, io.EOF) {
			return Frame{}, err
		}
		if len(rawLine) > 0 {
			raw = append(raw, rawLine...)
			if line := trimEOL(rawLine); len(line) > 0 {
				event, data = applyLine(line, event, data)
			} else if len(data) > 0 {
				return buildFrame(event, data, raw), nil
			} else {
				// 心跳或空块：丢弃已累积的原文字节，从下一块重新开始。
				event, data, raw = "", nil, nil
			}
		}
		if errors.Is(err, io.EOF) {
			if len(data) > 0 {
				return buildFrame(event, data, raw), nil
			}
			return Frame{}, io.EOF
		}
	}
}

// readLine 读取一行，返回的字节包含行尾换行符（若有）、不含则说明流已结束。
// limit 是本次调用允许累积的最大字节数，超限返回 ErrFrameTooLarge。
// 流在行中结束时返回已读到的内容与 io.EOF。
func (r *Reader) readLine(limit int) ([]byte, error) {
	var line []byte
	for {
		fragment, err := r.br.ReadSlice('\n')
		line = append(line, fragment...)
		if len(fragment) > 0 && r.onActivity != nil {
			r.onActivity()
		}
		if len(line) > limit {
			return nil, fmt.Errorf("%w: 上限 %d 字节", ErrFrameTooLarge, maxFrameBytes)
		}
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			// 行超过初始缓冲，继续按片段累积。
		default:
			return line, err
		}
	}
}

// applyLine 把一行字段并入当前帧的事件名与 data 正文。
// 行首为 `:` 的注释行、无冒号或非 event/data 的字段一律忽略。
func applyLine(line []byte, event string, data []byte) (string, []byte) {
	if line[0] == ':' {
		return event, data
	}
	colon := bytes.IndexByte(line, ':')
	if colon < 0 {
		return event, data
	}
	value := line[colon+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	switch string(line[:colon]) {
	case eventField:
		event = string(value)
	case dataField:
		data = append(data, value...)
		data = append(data, '\n')
	}
	return event, data
}

// trimEOL 去掉行尾的换行符，CRLF 与 LF 都支持。
func trimEOL(line []byte) []byte {
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	return line
}

// buildFrame 组装一条帧，并去掉 data 正文末尾由拼接产生的那一个换行符。
func buildFrame(event string, data, raw []byte) Frame {
	if n := len(data); n > 0 && data[n-1] == '\n' {
		data = data[:n-1]
	}
	return Frame{Event: event, Data: data, Raw: raw}
}
