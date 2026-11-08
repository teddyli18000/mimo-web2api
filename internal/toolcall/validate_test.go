package toolcall

import (
	"strings"
	"testing"

	"github.com/teddyli18000/mimo-web2api/internal/adapter"
)

func schema(name string, required ...string) adapter.OpenAITool {
	req := make([]interface{}, 0, len(required))
	for _, r := range required {
		req = append(req, r)
	}
	return adapter.OpenAITool{
		Type: "function",
		Function: adapter.OpenAIToolFunction{
			Name:       name,
			Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "required": req},
		},
	}
}

// 真机实测：模型只吐 `<invoke name="glob"></invoke>` 时，网关必须识别出
// 缺必填参数，而不是把 arguments="{}" 原样交给客户端。
func TestMissingRequired(t *testing.T) {
	tools := []adapter.OpenAITool{schema("glob", "pattern"), schema("read", "file_path")}

	cases := []struct {
		name  string
		calls []ParsedToolCall
		want  string
	}{
		{"没有调用", nil, ""},
		{"空 invoke", []ParsedToolCall{{Name: "glob", Input: map[string]any{}}}, "glob.pattern"},
		{"参数齐全", []ParsedToolCall{{Name: "glob", Input: map[string]any{"pattern": "**/*"}}}, ""},
		{"只给一个可选参数", []ParsedToolCall{{Name: "read", Input: map[string]any{"offset": float64(1)}}}, "read.file_path"},
		{"Input 为 nil", []ParsedToolCall{{Name: "read", Input: nil}}, "read.file_path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := strings.Join(MissingRequired(c.calls, tools), ",")
			if got != c.want {
				t.Fatalf("MissingRequired = %q, want %q", got, c.want)
			}
		})
	}
}

// 模型凭记忆猜工具名（read_file / list_dir）时，客户端查表必然失败，
// 必须像缺必填参数一样识别出来并重问。
func TestMissingRequiredFlagsUnknownTools(t *testing.T) {
	tools := []adapter.OpenAITool{schema("glob", "pattern")}
	calls := []ParsedToolCall{{Name: "read_file", Input: map[string]any{"path": "README.md"}}}
	got := MissingRequired(calls, tools)
	if len(got) != 1 || !strings.Contains(got[0], "unknown tool") {
		t.Fatalf("未声明的工具应被标出，实际 %v", got)
	}
}

func TestCorrectionNote(t *testing.T) {
	if got := CorrectionNote(nil); got != "" {
		t.Fatalf("空列表不该生成提示: %q", got)
	}
	got := CorrectionNote([]string{"glob.pattern", "read.file_path"})
	for _, want := range []string{"[Correction]", "glob.pattern", "read.file_path", "<parameter", "*"} {
		if !strings.Contains(got, want) {
			t.Fatalf("纠正提示缺少 %q: %s", want, got)
		}
	}
}

// 真机实测：模型把包装标签与内部标签的闭合写串，只留半截。
// 清洗后 content 不能有任何标签残片，否则客户端会把 XML 当正文显示。
func TestStripToolCallSyntaxCleansMismatchedClosers(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"错配闭合", "<tool_calls>\n\n\n</function>", ""},
		{"说明文字 + 错配闭合", "好的，我来查一下。\n<tool_calls>\n\n</function>", "好的，我来查一下。"},
		{"孤立包装标签", "前文 <tool_calls> 后文", "前文  后文"},
		{"DSML 残片", "前文 </|DSML|invoke> 后文", "前文  后文"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := StripToolCallSyntax(tc.in)
			if got != tc.want {
				t.Fatalf("StripToolCallSyntax(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "<tool_") || strings.Contains(got, "</function") {
				t.Fatalf("仍有标签残片: %q", got)
			}
		})
	}
}

func TestParseBareInvokeHasEmptyArgs(t *testing.T) {
	text := "<tool_calls>\n<invoke name=\"glob\"></invoke>\n</tool_calls>"
	calls := ParseToolCallsFromText(text)
	if len(calls) != 1 {
		t.Fatalf("应解析出 1 个调用，实际 %d", len(calls))
	}
	if len(calls[0].Input) != 0 {
		t.Fatalf("空 invoke 的参数应为空，实际 %v", calls[0].Input)
	}
	if got := MissingRequired(calls, []adapter.OpenAITool{schema("glob", "pattern")}); len(got) != 1 {
		t.Fatalf("应识别出缺 pattern，实际 %v", got)
	}
}
