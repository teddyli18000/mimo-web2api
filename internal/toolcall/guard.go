package toolcall

import (
	"strings"
	"unicode/utf8"
)

// MarkerGuard 在把上游正文增量转发给客户端时，扣押末尾一小段文本。
//
// 上游返回的是「约定格式的纯文本」，工具调用要靠 HasToolCallSyntax /
// ParseToolCallsFromText 事后解析。真流式下如果原样转发，`<tool_calls>`、
// `<|DSML|invoke>`、`% ToolName ...` 这些内部标记会先一步漏给客户端，
// 等解析出来再补救已经来不及。
//
// 策略：
//   - 每次追加后先检查缓冲区里有没有标记特征；命中就进入「扣押模式」，
//     此后不再外发，全部留在缓冲区等结尾整体解析。
//   - 未命中时只扣住「末尾正好是某个标记前缀」的那几个字节，其余立即外发。
//     不能用一个固定大小的窗口（例如 32 字节）：短回答（几段中文）会一直
//     小于窗口，结果全被扣到流结束——看起来又变回了伪流式。
//   - 切分点回退到完整 UTF-8 边界，否则 JSON 序列化会把半个汉字变成 U+FFFD。
//   - 结尾 Flush 取出剩余文本；若解析不出工具调用（误判），调用方补发即可。
//
// MarkerGuard 不是并发安全的：一个客户端流配一个实例。
type MarkerGuard struct {
	buf     []byte
	blocked bool
	marker  string // 命中的标记特征，仅用于日志
}

func NewMarkerGuard() *MarkerGuard {
	return &MarkerGuard{}
}

// Write 追加一段正文增量，返回可以立即下发的安全部分。
// blocked 表示已经进入扣押模式（含本次触发）。
func (g *MarkerGuard) Write(chunk string) (safe string, blocked bool) {
	if chunk == "" {
		return "", g.blocked
	}
	g.buf = append(g.buf, chunk...)
	if g.blocked {
		return "", true
	}
	if HasToolCallSyntax(string(g.buf)) {
		g.blocked = true
		g.marker = markerKind(g.buf)
		return "", true
	}
	cut := len(g.buf) - holdbackLen(g.buf)
	if cut <= 0 {
		return "", false
	}
	cut = utf8SafeCut(g.buf, cut)
	if cut <= 0 {
		return "", false
	}
	safe = string(g.buf[:cut])
	g.buf = append(g.buf[:0], g.buf[cut:]...)
	return safe, false
}

// holdbackLen 返回「必须继续扣住、否则可能把标记切断」的尾部字节数。
//
// 只扣住能被补成标记的那些后缀：比如末尾是 "tool_ca" 就再等几个字节，
// 末尾是「第一段」则一个字节都不扣，立刻下发。
func holdbackLen(buf []byte) int {
	if len(buf) == 0 {
		return 0
	}
	lower := strings.ToLower(string(buf))
	best := 0
	for _, m := range toolMarkers {
		max := len(m) - 1 // 完整匹配已经在上层判过，这里只可能是前缀
		if max > len(lower) {
			max = len(lower)
		}
		for n := max; n > best; n-- {
			if strings.HasSuffix(lower, m[:n]) {
				best = n
				break
			}
		}
	}
	// 行首的 "% ToolName" 也是工具语法：单个 % 先扣住，看后面是不是跟了名字
	if best < 1 && lower[len(lower)-1] == '%' {
		best = 1
	}
	// 标记的开头（<function=、<|DSML|）连同尖括号/竖线一起扣住，
	// 否则客户端会先看到一个孤零零的 "<"。
	for best > 0 {
		i := len(buf) - best - 1
		if i < 0 || (buf[i] != '<' && buf[i] != '|') {
			break
		}
		best++
	}
	return best
}

// Flush 取出仍在缓冲区里的文本（流结束时调用）。
// 调用后缓冲区清空，但 blocked 状态保留，便于调用方判断是否命中过标记。
func (g *MarkerGuard) Flush() string {
	out := string(g.buf)
	g.buf = g.buf[:0]
	return out
}

// FlushPrefix 取出命中标记之前的正文（不含标记及其后的内容）。
// 未命中标记时等同 Flush。
//
// 用途：模型常常先写一句「我来查一下」再吐工具标记；这句话不能因为
// 守卫扣押而丢掉，解析出工具调用后要先把它补发给客户端。
func (g *MarkerGuard) FlushPrefix() string {
	if !g.blocked {
		return g.Flush()
	}
	idx := markerIndex(g.buf)
	if idx < 0 {
		idx = 0
	}
	idx = utf8SafeCut(g.buf, idx)
	out := string(g.buf[:idx])
	g.buf = append(g.buf[:0], g.buf[idx:]...)
	return out
}

// Blocked 是否命中过工具标记。
func (g *MarkerGuard) Blocked() bool { return g.blocked }

// Marker 返回命中的标记特征（"tool_calls" / "dsml" / "function=" / "tool_call" / "%tool"）。
// 只用于日志，不含正文。
func (g *MarkerGuard) Marker() string { return g.marker }

// Buffered 当前扣押的文本长度（用于诊断日志）。
func (g *MarkerGuard) Buffered() int { return len(g.buf) }

// utf8SafeCut 把切分点回退到 UTF-8 字符边界。
func utf8SafeCut(b []byte, cut int) int {
	if cut <= 0 {
		return 0
	}
	if cut >= len(b) {
		return len(b)
	}
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return cut
}

// markerKind 返回命中的标记特征（仅用于日志，不含正文）。
var toolMarkers = []string{"tool_calls", "dsml", "function=", "tool_call"}

func markerKind(buf []byte) string {
	lower := strings.ToLower(string(buf))
	for _, m := range toolMarkers {
		if strings.Contains(lower, m) {
			return m
		}
	}
	return "%tool"
}

// markerIndex 返回最早出现的标记位置；没有返回 -1。
func markerIndex(buf []byte) int {
	lower := strings.ToLower(string(buf))
	best := -1
	for _, m := range toolMarkers {
		if i := strings.Index(lower, m); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	// `% ToolName ...` 形态：行首的百分号
	if i := strings.Index(lower, "\n%"); i >= 0 {
		i++
		if best < 0 || i < best {
			best = i
		}
	}
	if strings.HasPrefix(lower, "%") {
		best = 0
	}
	// 标记前的尖括号/竖线也是标记的一部分（`<function=`、`<|DSML|tool_calls>`），
	// 否则补发前缀时会甩一个孤零零的 "<" 给客户端。
	for best > 0 && (buf[best-1] == '<' || buf[best-1] == '|') {
		best--
	}
	return best
}
