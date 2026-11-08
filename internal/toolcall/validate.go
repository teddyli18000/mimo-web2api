package toolcall

import (
	"sort"
	"strings"

	"github.com/teddyli18000/mimo-web2api/internal/adapter"
)

// MissingRequired 检查解析出的工具调用是否漏了必填参数。
//
// 上游模型偶尔只吐 `<invoke name="glob"></invoke>` 就把调用结束掉，网关照原样
// 转成 tool_calls 时 arguments 会是 `{}`，客户端校验必填项直接判失败
// （DSH 报 missing required property "pattern"）。与其把残缺调用发给客户端，
// 不如在这里识别出来，让调用方带着纠正提示重问一次。
//
// 返回形如 ["glob.pattern", "read.file_path"] 的列表；空列表表示全部齐全。
func MissingRequired(calls []ParsedToolCall, tools []adapter.OpenAITool) []string {
	if len(calls) == 0 {
		return nil
	}
	required := make(map[string][]string, len(tools))
	known := make(map[string]bool, len(tools))
	for _, t := range tools {
		name := strings.TrimSpace(t.Function.Name)
		if name == "" {
			continue
		}
		known[name] = true
		params, ok := t.Function.Parameters.(map[string]interface{})
		if !ok {
			continue
		}
		keys := requiredKeys(params)
		if len(keys) > 0 {
			required[name] = keys
		}
	}
	if len(known) == 0 {
		return nil
	}

	var missing []string
	seen := make(map[string]bool)
	add := func(label string) {
		if !seen[label] {
			seen[label] = true
			missing = append(missing, label)
		}
	}
	for _, call := range calls {
		// 模型凭记忆猜工具名（read_file / list_dir / run_command）时，
		// 客户端查表必然失败。这种调用和缺必填参数一样，都该重问而不是放行。
		if !known[call.Name] {
			add(call.Name + " (unknown tool)")
			continue
		}
		for _, key := range required[call.Name] {
			if _, ok := call.Input[key]; ok {
				continue
			}
			add(call.Name + "." + key)
		}
	}
	return missing
}

// requiredKeys 从 JSON schema 里取 required 列表；客户端既可能给
// []interface{}（encoding/json 反序列化），也可能给 []string（手工构造）。
func requiredKeys(params map[string]interface{}) []string {
	var keys []string
	switch r := params["required"].(type) {
	case []interface{}:
		for _, v := range r {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				keys = append(keys, s)
			}
		}
	case []string:
		for _, s := range r {
			if strings.TrimSpace(s) != "" {
				keys = append(keys, s)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

// CorrectionNote 生成重问时追加的提示，明确点名漏掉的参数。
func CorrectionNote(missing []string) string {
	if len(missing) == 0 {
		return ""
	}
	return "\n\n[Correction] Your previous tool call was rejected: " +
		strings.Join(missing, ", ") +
		". Use a tool name that appears in the tool list below, verbatim, and re-emit the complete <tool_calls> block " +
		"with one <parameter name=\"...\">VALUE</parameter> line for EVERY parameter marked * in that tool's signature. " +
		"An <invoke> with no parameters is invalid. Do not answer in prose."
}
