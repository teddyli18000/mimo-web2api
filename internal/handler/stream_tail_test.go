package handler

import (
	"testing"
)

// 真机实测：模型吐了一个没有任何 invoke 的空工具块，MarkerGuard 扣住后
// 解析出 0 个调用，走「误判补发」把内部标记原样发给客户端。
func TestCleanStreamTail(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"空 tool_calls 块", "正文。\n<tool_calls>\n</tool_calls>", "正文。"},
		{"错配闭合残片", "正文。<tool_calls>\n\n</function>", "正文。"},
		{"含 invoke 的残片", "正文。\n<tool_calls>\n<invoke name=\"glob\"></invoke>\n</tool_calls>", "正文。"},
		// 只提到关键词、没有实际标签时，一个字都不能动
		{"纯关键词不动", "请解释 tool_calls 这个词的来历。", "请解释 tool_calls 这个词的来历。"},
		{"空串", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cleanStreamTail(c.in); got != c.want {
				t.Fatalf("cleanStreamTail(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// 空工具块被当作误判补发时，客户端不该看到内部标记；
// 但正文内容必须完整保留，不能因为清洗把整段话丢掉。
func TestCleanStreamTailKeepsProse(t *testing.T) {
	in := "glob 用于按路径模式查找文件，read 用于读取文件内容。\n<tool_calls>\n</tool_calls>"
	got := cleanStreamTail(in)
	if got != "glob 用于按路径模式查找文件，read 用于读取文件内容。" {
		t.Fatalf("正文被破坏: %q", got)
	}
}
