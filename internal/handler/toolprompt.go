package handler

import (
	"fmt"
	"sort"
	"strings"

	"github.com/teddyli18000/mimo-web2api/internal/adapter"
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
			pDesc := oneLine(paramDescription(props[pname]), 100)
			if pDesc == "" {
				continue
			}
			sb.WriteString(fmt.Sprintf("    %s: %s\n", pname, pDesc))
		}
	}
	return sb.String()
}

// toolParams 取出 parameters.properties 与 required 集合。
// 客户端传进来的 Parameters 是 map[string]interface{}（JSON 反序列化结果）。
func toolParams(tool adapter.OpenAITool) (map[string]interface{}, map[string]bool) {
	props := map[string]interface{}{}
	required := map[string]bool{}

	params, ok := tool.Function.Parameters.(map[string]interface{})
	if !ok {
		return props, required
	}
	if p, ok := params["properties"].(map[string]interface{}); ok {
		props = p
	}
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
	return props, required
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
