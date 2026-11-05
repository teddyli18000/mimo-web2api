package toolcall

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 普通正文：除了末尾保留窗口，其余必须立刻下发；Flush 后与原文完全一致。
func TestMarkerGuardStreamsPlainText(t *testing.T) {
	g := NewMarkerGuard()
	var out strings.Builder
	for _, chunk := range []string{"你好", "，世界", "！这是一段很长的普通回答，没有任何工具标记。"} {
		safe, blocked := g.Write(chunk)
		if blocked {
			t.Fatalf("普通正文被误判成工具调用: %q", chunk)
		}
		out.WriteString(safe)
	}
	out.WriteString(g.Flush())

	want := "你好，世界！这是一段很长的普通回答，没有任何工具标记。"
	if out.String() != want {
		t.Fatalf("拼接结果不对\n got: %q\nwant: %q", out.String(), want)
	}
}

// 标记被切碎逐字喂进来：命中前不能把标记前半截漏出去。
func TestMarkerGuardHoldsSplitMarker(t *testing.T) {
	g := NewMarkerGuard()
	text := "先说明一下。<function=WebFetch><parameter name=\"url\">x</parameter></function>"

	var out strings.Builder
	for _, r := range text {
		safe, _ := g.Write(string(r))
		out.WriteString(safe)
	}

	if !g.Blocked() {
		t.Fatal("跨 rune 喂入的标记没有被识别")
	}
	if got := g.Marker(); got != "function=" {
		t.Fatalf("命中的标记特征不对: %q", got)
	}
	if strings.Contains(out.String(), "function") {
		t.Fatalf("标记前半截已经漏给客户端: %q", out.String())
	}

	// 已下发 + 扣押 = 原文，不能丢字
	if all := out.String() + g.Flush(); all != text {
		t.Fatalf("丢字了\n got: %q\nwant: %q", all, text)
	}
}

// 误判（正文里只是提到了 tool_calls）：调用方 Flush 就能把内容补全。
func TestMarkerGuardFalsePositiveStillFlushes(t *testing.T) {
	g := NewMarkerGuard()
	safe, blocked := g.Write("文档里提到了 tool_calls 这个词。")
	if safe != "" {
		t.Fatalf("命中标记时不应立即下发: %q", safe)
	}
	if !blocked {
		t.Fatal("应该进入扣押模式")
	}
	rest := g.Flush()
	if rest != "文档里提到了 tool_calls 这个词。" {
		t.Fatalf("Flush 内容不对: %q", rest)
	}
}

// 按字节切中文：下发片段必须是完整 UTF-8，否则 JSON 序列化会把半个汉字变成 U+FFFD。
func TestMarkerGuardUTF8Boundary(t *testing.T) {
	g := NewMarkerGuard()
	text := strings.Repeat("中文内容", 20)

	var out strings.Builder
	for i := 0; i < len(text); i += 3 {
		end := i + 3
		if end > len(text) {
			end = len(text)
		}
		safe, _ := g.Write(text[i:end])
		if !utf8.ValidString(safe) {
			t.Fatalf("下发了半个字符: %q", safe)
		}
		out.WriteString(safe)
	}
	out.WriteString(g.Flush())

	if out.String() != text {
		t.Fatal("按字节切分后内容不一致")
	}
}

// FlushPrefix 只吐标记之前的正文，标记本身留在缓冲区。
func TestMarkerGuardFlushPrefix(t *testing.T) {
	g := NewMarkerGuard()
	g.Write("好的，我来查一下。")
	g.Write("<function=WebFetch>")

	pre := g.FlushPrefix()
	if pre != "好的，我来查一下。" {
		t.Fatalf("前缀不对: %q", pre)
	}
	if g.Flush() != "<function=WebFetch>" {
		t.Fatal("标记本身应该留在缓冲区")
	}
}
