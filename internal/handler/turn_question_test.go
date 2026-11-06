package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teddyli18000/mimo-web2api/internal/adapter"
)

func msg(role, text string) adapter.OpenAIMessage {
	return adapter.OpenAIMessage{Role: role, Content: text}
}

func amsg(role string, content interface{}) adapter.AnthropicMessage {
	return adapter.AnthropicMessage{Role: role, Content: content}
}

func TestCurrentTurnQuestionAnthropic(t *testing.T) {
	toolResult := []interface{}{
		map[string]interface{}{"type": "tool_result", "tool_use_id": "t1", "content": "命令输出"},
	}
	cases := []struct {
		name string
		msgs []adapter.AnthropicMessage
		want string
	}{
		{
			name: "首轮提问",
			msgs: []adapter.AnthropicMessage{amsg("user", "帮我看看日志")},
			want: "帮我看看日志",
		},
		{
			name: "工具结果轮不把工具输出当提问",
			msgs: []adapter.AnthropicMessage{
				amsg("user", "帮我看看日志"),
				amsg("assistant", "好的"),
				amsg("user", toolResult),
			},
			want: "",
		},
		{
			name: "工具结果后追加新指令",
			msgs: []adapter.AnthropicMessage{
				amsg("user", "帮我看看日志"),
				amsg("assistant", "好的"),
				amsg("user", toolResult),
				amsg("user", "顺便把配置也检查一下"),
			},
			want: "顺便把配置也检查一下",
		},
		{
			name: "注入上下文不算提问",
			msgs: []adapter.AnthropicMessage{
				amsg("assistant", "上轮回答"),
				amsg("user", "<system-reminder>只有注入</system-reminder>"),
			},
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := currentTurnQuestionAnthropic(c.msgs); got != c.want {
				t.Errorf("currentTurnQuestionAnthropic() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestIsAnthropicToolResult(t *testing.T) {
	onlyResult := []interface{}{map[string]interface{}{"type": "tool_result", "content": "x"}}
	mixed := []interface{}{
		map[string]interface{}{"type": "tool_result", "content": "x"},
		map[string]interface{}{"type": "text", "text": "补充说明"},
	}
	if !isAnthropicToolResult(amsg("user", onlyResult)) {
		t.Error("纯 tool_result 应判定为工具结果")
	}
	if isAnthropicToolResult(amsg("user", mixed)) {
		t.Error("含文本块的混合消息不应判定为纯工具结果")
	}
	if isAnthropicToolResult(amsg("user", "普通文本")) {
		t.Error("字符串内容不应判定为工具结果")
	}
	if isAnthropicToolResult(amsg("user", []interface{}{})) {
		t.Error("空块列表不应判定为工具结果")
	}
}

func TestWriteAnthropicErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAnthropicError(rec, http.StatusBadRequest, "bad input")

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Type != "error" {
		t.Errorf("type = %q, want error", body.Type)
	}
	if body.Error.Type != "invalid_request_error" {
		t.Errorf("error.type = %q, want invalid_request_error", body.Error.Type)
	}
	if body.Error.Message != "bad input" {
		t.Errorf("error.message = %q", body.Error.Message)
	}
}

func TestCurrentTurnQuestion(t *testing.T) {
	cases := []struct {
		name string
		msgs []adapter.OpenAIMessage
		want string
	}{
		{
			name: "首轮：system + 提问",
			msgs: []adapter.OpenAIMessage{msg("system", "sys"), msg("user", "帮我查日志")},
			want: "帮我查日志",
		},
		{
			name: "首轮：提问在前、注入在后（DSH 形态）",
			msgs: []adapter.OpenAIMessage{
				msg("system", "sys"),
				msg("user", "插件市场卡住了"),
				msg("user", "<system-reminder>AGENTS.md…</system-reminder>"),
				msg("user", "Current runtime context. workspace-write"),
			},
			want: "插件市场卡住了",
		},
		{
			name: "工具结果轮：没有新提问",
			msgs: []adapter.OpenAIMessage{
				msg("system", "sys"),
				msg("user", "插件市场卡住了"),
				msg("assistant", "[called pwsh]"),
				msg("tool", "命令输出"),
			},
			want: "",
		},
		{
			name: "第二轮：assistant 之后的新提问",
			msgs: []adapter.OpenAIMessage{
				msg("system", "sys"),
				msg("user", "第一个问题"),
				msg("assistant", "回答"),
				msg("user", "第二个问题"),
			},
			want: "第二个问题",
		},
		{
			name: "本轮以注入开头（没有真实提问）",
			msgs: []adapter.OpenAIMessage{
				msg("system", "sys"),
				msg("assistant", "回答"),
				msg("user", "<system-reminder>只有注入</system-reminder>"),
			},
			want: "",
		},
		{
			name: "空列表",
			msgs: nil,
			want: "",
		},
		{
			name: "只有 system",
			msgs: []adapter.OpenAIMessage{msg("system", "sys")},
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := currentTurnQuestion(c.msgs); got != c.want {
				t.Errorf("currentTurnQuestion() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestBuildToolPromptCarriesParamSpec(t *testing.T) {
	tools := []adapter.OpenAITool{{
		Type: "function",
		Function: adapter.OpenAIToolFunction{
			Name:        "pwsh",
			Description: "Run a PowerShell command.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command":     map[string]interface{}{"type": "string", "description": "The PowerShell command to execute."},
					"description": map[string]interface{}{"type": "string", "description": "Clear, concise description of what this command does."},
					"timeout":     map[string]interface{}{"type": "number"},
				},
				"required": []interface{}{"command", "description"},
			},
		},
	}}
	got := buildToolPrompt(tools)

	// 必填参数必须标注（* 后缀），否则模型会漏传（2026-09 实测 missing required property）
	for _, want := range []string{
		"command*:string",
		"description*:string",
		"timeout:number",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("工具提示缺少 %q\n实际内容:\n%s", want, got)
		}
	}
	if strings.Contains(got, "timeout*") {
		t.Error("可选参数被误标为必填")
	}
	// 参数说明决定模型能不能填对值，必填项必须带
	for _, want := range []string{
		"The PowerShell command to execute.",
		"Clear, concise description of what this command does.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("必填参数缺少说明 %q\n实际内容:\n%s", want, got)
		}
	}

	// 调用格式契约必须在清单之前：超预算时截尾，契约得活下来
	contract := strings.Index(got, "<tool_calls>")
	list := strings.Index(got, "Available tools:")
	if contract < 0 || list < 0 || contract > list {
		t.Fatalf("格式契约必须排在工具清单之前（contract=%d list=%d）\n%s", contract, list, got)
	}
	// 数值/布尔/数组的写法必须交代清楚，否则模型会给数字加引号
	if !strings.Contains(got, "raw JSON") {
		t.Error("缺少非字符串参数的写法说明")
	}
}

// 预算吃紧时，最先被牺牲的是历史与用户 system，工具契约必须完整保留。
func TestSerializeKeepsToolContractUnderBudget(t *testing.T) {
	tools := buildToolPrompt([]adapter.OpenAITool{{
		Type: "function",
		Function: adapter.OpenAIToolFunction{
			Name:       "read",
			Parameters: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"file_path": map[string]interface{}{"type": "string"}}, "required": []interface{}{"file_path"}},
		},
	}})

	rts := []roleText{
		{"system", strings.Repeat("SYSTEM-RULE ", 400)},
		{"user", "看一下仓库"},
		{"assistant", strings.Repeat("history ", 400)},
		{"user", "现在列一下目录"},
	}
	got := serializeQuery(rts, 4000, tools)

	if !strings.Contains(got, "<tool_calls>") || !strings.Contains(got, "<invoke name=") {
		t.Fatalf("工具契约被截掉了:\n%s", got)
	}
	if !strings.Contains(got, "read(file_path*:string)") {
		t.Fatalf("工具清单被整段丢掉:\n%s", got)
	}
	if !strings.Contains(got, "[Current Query]") || !strings.Contains(got, "现在列一下目录") {
		t.Fatalf("提问必须保留:\n%s", got)
	}
	if n := len([]rune(got)); n > 4000 {
		t.Fatalf("超出预算: %d", n)
	}
}

func TestLooksLikeInjectedContext(t *testing.T) {
	injected := []string{
		"<system-reminder>\nThe following workspace instructions…",
		"Current runtime context. This snapshot supersedes…",
		"Current DSH file policy: workspace-write.",
	}
	for _, s := range injected {
		if !looksLikeInjectedContext(s) {
			t.Errorf("应判定为注入上下文: %q", s)
		}
	}
	real := []string{
		"帮我看看插件市场为什么卡住",
		"我的dsh的插件市场没法更新",
		"<div>这不是 reminder</div>",
	}
	for _, s := range real {
		if looksLikeInjectedContext(s) {
			t.Errorf("不应判定为注入上下文: %q", s)
		}
	}
}
