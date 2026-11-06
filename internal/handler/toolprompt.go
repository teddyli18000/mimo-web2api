package handler

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/teddyli18000/mimo-web2api/internal/adapter"
	"github.com/teddyli18000/mimo-web2api/internal/toolcall"
)

// toolFormatContract 工具调用的输出契约。
//
// 它必须排在工具清单**前面**：serializeRoleTexts 超预算时从尾部截断，
// 早先的版本把格式说明放在最后，于是工具一多（DSH 会注册 50 个）或历史一长，
// 被切掉的正好是"该怎么输出工具调用"这段——模型只能凭自己的习惯猜工具名
// （read_file / list_dir / run_command），客户端一个都认不出来，整轮工具调用全废。
const toolFormatContract = `You have the following tools. When one applies you MUST call it instead of answering from your own knowledge — your knowledge may be stale and the tool returns live data.

Call format (emit this block verbatim in your reply):

<tool_calls>
<invoke name="TOOL_NAME">
<parameter name="PARAM_NAME">VALUE</parameter>
</invoke>
</tool_calls>

Rules:
- Use the exact tool name and parameter names from the list below; a call with an unknown name or a missing required parameter (marked *) is rejected.
- Strings are written as-is. Numbers, booleans, arrays and objects must be raw JSON: 42, true, ["a","b"], {"k":1} — do not quote numbers or booleans.
- Several <invoke> blocks inside one <tool_calls> block call several tools at once.
- Do not describe a call instead of making it. After the tool result comes back, continue the task.
`

// buildToolPrompt 渲染工具契约 + 工具清单。
//
// 每个工具都给出完整的参数名/类型/必填标记，必填参数再补一行说明：
// 只写参数名时模型经常漏必填项（2026-09 实测 DSH 会话反复出现
// missing required property "description"/"pattern"），而"这个参数是什么意思"
// 恰恰是模型猜不出来的部分。
func buildToolPrompt(tools []adapter.OpenAITool) string {
	if len(tools) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(toolFormatContract)
	sb.WriteString("\nAvailable tools:\n")

	for _, tool := range tools {
		name := strings.TrimSpace(tool.Function.Name)
		if name == "" {
			continue
		}
		desc := oneLine(tool.Function.Description, 160)
		props, required := toolParams(tool)
		names := make([]string, 0, len(props))
		for pname := range props {
			names = append(names, pname)
		}
		sort.Strings(names)

		// 签名行：read(file_path*:string, offset:number, limit:number)
		sig := make([]string, 0, len(names))
		for _, pname := range names {
			pType := paramType(props[pname])
			mark := ""
			if required[pname] {
				mark = "*"
			}
			sig = append(sig, fmt.Sprintf("%s%s:%s", pname, mark, pType))
		}
		if desc != "" {
			sb.WriteString(fmt.Sprintf("- %s(%s) — %s\n", name, strings.Join(sig, ", "), desc))
		} else {
			sb.WriteString(fmt.Sprintf("- %s(%s)\n", name, strings.Join(sig, ", ")))
		}

		// 参数说明只对必填项展开：可选参数漏了不影响调用，
		// 必填项漏了就是一次失败调用，而完整展开 50 个工具会吃掉大半预算。
		for _, pname := range names {
			if !required[pname] {
				continue
			}
			parts := make([]string, 0, 2)
			// 对象/数组参数光有类型没用：模型不知道里面该放什么键，
			// 于是对着 questions / files 这种参数编一个结构，客户端校验直接拒。
			if shape := nestedShape(props[pname]); shape != "" {
				parts = append(parts, shape)
			}
			if pDesc := oneLine(paramDescription(props[pname]), 100); pDesc != "" {
				parts = append(parts, pDesc)
			}
			if len(parts) == 0 {
				continue
			}
			sb.WriteString(fmt.Sprintf("    %s: %s\n", pname, strings.Join(parts, " — ")))
		}
	}
	return sb.String()
}

// toolParams 取出 parameters.properties 与 required 集合。
// 客户端传进来的 Parameters 是 map[string]interface{}（JSON 反序列化结果）。
func toolParams(tool adapter.OpenAITool) (map[string]interface{}, map[string]bool) {
	props := map[string]interface{}{}

	params, ok := tool.Function.Parameters.(map[string]interface{})
	if !ok {
		return props, map[string]bool{}
	}
	if p, ok := params["properties"].(map[string]interface{}); ok {
		props = p
	}
	return props, requiredSet(params)
}

// requiredSet 读出 schema 里的 required 列表。
func requiredSet(params map[string]interface{}) map[string]bool {
	required := map[string]bool{}
	switch r := params["required"].(type) {
	case []interface{}:
		for _, v := range r {
			if s, ok := v.(string); ok {
				required[s] = true
			}
		}
	case []string:
		for _, s := range r {
			required[s] = true
		}
	}
	return required
}

func paramType(raw interface{}) string {
	pm, ok := raw.(map[string]interface{})
	if !ok {
		return "any"
	}
	if t, ok := pm["type"].(string); ok && t != "" {
		return t
	}
	// 有些客户端用 anyOf/oneOf 表达联合类型
	if _, ok := pm["anyOf"]; ok {
		return "any"
	}
	return "any"
}

// nestedShape 给对象/数组参数渲染一层内部结构，例如：
//
//	questions (array) → "array of {question*:string, options*:array, header:string}"
//
// 只展开一层：再深就会把提示词撑爆，而模型对第二层之后的准确率本来就很低。
func nestedShape(raw interface{}) string {
	pm, ok := raw.(map[string]interface{})
	if !ok {
		return ""
	}
	kind, _ := pm["type"].(string)
	switch kind {
	case "array":
		items, ok := pm["items"].(map[string]interface{})
		if !ok {
			return ""
		}
		if shape := objectShape(items); shape != "" {
			return "array of " + shape
		}
	case "object":
		return objectShape(pm)
	}
	return ""
}

func objectShape(m map[string]interface{}) string {
	props, ok := m["properties"].(map[string]interface{})
	if !ok || len(props) == 0 {
		return ""
	}
	required := requiredSet(m)
	names := make([]string, 0, len(props))
	for pname := range props {
		names = append(names, pname)
	}
	sort.Strings(names)
	if len(names) > 6 {
		names = names[:6]
	}
	parts := make([]string, 0, len(names)+1)
	for _, pname := range names {
		mark := ""
		if required[pname] {
			mark = "*"
		}
		parts = append(parts, pname+mark+":"+paramType(props[pname]))
	}
	shape := "{" + strings.Join(parts, ", ")
	if len(props) > len(names) {
		shape += ", …"
	}
	return shape + "}"
}

func paramDescription(raw interface{}) string {
	pm, ok := raw.(map[string]interface{})
	if !ok {
		return ""
	}
	if d, ok := pm["description"].(string); ok {
		return d
	}
	return ""
}

// normalizeToolNames 把模型写的工具名对齐到请求里声明的名字。
//
// 只做「大小写不同」这种无损纠正：客户端按名字查表，大小写不匹配一样查不到。
// 真的查不到就原样返回并记一条日志——直接丢掉这次调用会让模型永远等不到结果，
// 交给客户端报「未知工具」反而是能自我纠正的路径。
func normalizeToolNames(calls []toolcall.ParsedToolCall, tools []adapter.OpenAITool) []toolcall.ParsedToolCall {
	if len(calls) == 0 || len(tools) == 0 {
		return calls
	}
	known := make(map[string]bool, len(tools))
	byLower := make(map[string]string, len(tools))
	for _, t := range tools {
		name := t.Function.Name
		if name == "" {
			continue
		}
		known[name] = true
		byLower[strings.ToLower(name)] = name
	}
	for i := range calls {
		if known[calls[i].Name] {
			continue
		}
		if canon, ok := byLower[strings.ToLower(calls[i].Name)]; ok {
			log.Printf("[tools] 工具名大小写不一致：模型写了 %q，声明的是 %q", calls[i].Name, canon)
			calls[i].Name = canon
			continue
		}
		log.Printf("[tools] 模型调用了未声明的工具 %q（已声明 %d 个）", calls[i].Name, len(known))
	}
	return calls
}

// oneLine 压成单行并截断：工具描述常含多行 Markdown，换行会让 "- " 条目
// 在模型眼里变成同级的新工具，后面的参数行也会被吸附到描述末尾。
func oneLine(s string, limit int) string {
	t := strings.Join(strings.Fields(s), " ")
	r := []rune(t)
	if len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return t
}
