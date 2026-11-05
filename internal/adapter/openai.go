package adapter

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// OpenAIChatRequest 是 OpenAI 格式的请求
type OpenAIChatRequest struct {
	Model       string          `json:"model"`
	Messages    []OpenAIMessage `json:"messages"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Tools       []OpenAITool    `json:"tools,omitempty"`
	ToolChoice  interface{}     `json:"tool_choice,omitempty"`
	// ReasoningEffort: low / medium / high，显式开启思考并透传给上游
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// OpenAITool 是 OpenAI 格式的工具定义
type OpenAITool struct {
	Type     string             `json:"type"`
	Function OpenAIToolFunction `json:"function"`
}

// OpenAIToolFunction 是工具函数定义
type OpenAIToolFunction struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
}

// OpenAIMessage 是 OpenAI 格式的消息
// OpenAIMessage 是 OpenAI 格式的消息
type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    interface{}      `json:"content"` // string 或 []ContentPart
	// ReasoningContent 是上游 v3 的 reasoningContent（思考链）。DeepSeek 系客户端认这个字段，
	// 普通 OpenAI 客户端会忽略未知字段，所以只在非空时输出。
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
}

// OpenAIToolCall 是 OpenAI 格式的工具调用
type OpenAIToolCall struct {
	// Index 仅流式响应使用：客户端按它把分片的工具调用拼回完整调用
	Index *int `json:"index,omitempty"`
	// ID/Type/Name 在参数分片里必须省略：按真实 OpenAI 的形状，
	// 后续分片只带 index + function.arguments，重复 id/name 会让部分客户端
	// 误判为「又来了一个新调用」而把同一个工具执行两次。
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function OpenAIToolCallFunc `json:"function"`
}

// OpenAIToolCallFunc 是工具调用函数
type OpenAIToolCallFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// OpenAIChatResponse 是 OpenAI 格式的响应
type OpenAIChatResponse struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []OpenAIChoice   `json:"choices"`
	Usage   *OpenAIUsage     `json:"usage,omitempty"`
}

// OpenAIChoice 是 OpenAI 格式的选项
type OpenAIChoice struct {
	Index        int            `json:"index"`
	Message      *OpenAIMessage `json:"message,omitempty"`
	Delta        *OpenAIDelta   `json:"delta,omitempty"`
	FinishReason *string        `json:"finish_reason,omitempty"`
}

// OpenAIDelta 是流式增量
type OpenAIDelta struct {
	Role             string           `json:"role,omitempty"`
	Content          string           `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
}

// OpenAIUsage 是 token 使用量
type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// OpenAIStreamChunk 是流式响应块
type OpenAIStreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   *OpenAIUsage   `json:"usage,omitempty"`
}


// NewStreamID 生成流式响应 id。
// OpenAI 规范要求同一个流的全部 chunk 共用同一个 id（客户端据此归组）。
func NewStreamID() string {
	return fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:8])
}

func marshalChunk(id, model string, choices []OpenAIChoice, usage *OpenAIUsage) []byte {
	data, _ := json.Marshal(OpenAIStreamChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: choices,
		Usage:   usage,
	})
	return data
}

// MakeOpenAIStreamContentChunk 内容增量块
func MakeOpenAIStreamContentChunk(id, model, content string) []byte {
	return marshalChunk(id, model, []OpenAIChoice{
		{Index: 0, Delta: &OpenAIDelta{Content: content}},
	}, nil)
}

// MakeOpenAIStreamReasoningChunk 思考链增量块（reasoning_content）。
func MakeOpenAIStreamReasoningChunk(id, model, reasoning string) []byte {
	return marshalChunk(id, model, []OpenAIChoice{
		{Index: 0, Delta: &OpenAIDelta{ReasoningContent: reasoning}},
	}, nil)
}

// MakeOpenAIStreamRoleChunk 首帧：声明 assistant role。
//
// 真实 OpenAI 流的第一个 chunk 是 {"delta":{"role":"assistant"}}，
// 客户端据此初始化 assistant 消息；缺了它，部分 SDK 会把后续 content
// 当作「没有角色的片段」，在严格实现里直接报协议错误。
func MakeOpenAIStreamRoleChunk(id, model string) []byte {
	return marshalChunk(id, model, []OpenAIChoice{
		{Index: 0, Delta: &OpenAIDelta{Role: "assistant"}},
	}, nil)
}

// MakeOpenAIStreamFinishChunk 收尾块（finish_reason=stop）
func MakeOpenAIStreamFinishChunk(id, model string) []byte {
	return MakeOpenAIStreamFinishReasonChunk(id, model, "stop")
}

// MakeOpenAIStreamFinishReasonChunk 指定 finish_reason 的收尾块。
func MakeOpenAIStreamFinishReasonChunk(id, model, reason string) []byte {
	fr := reason
	return marshalChunk(id, model, []OpenAIChoice{
		{Index: 0, Delta: &OpenAIDelta{}, FinishReason: &fr},
	}, nil)
}

// MakeOpenAIStreamToolCallStartChunk 工具调用首片：只声明 id/type/name，arguments 为空。
//
// 真实 OpenAI 把工具调用拆成「先 name 再 arguments 增量」两阶段；
// 一次性塞完整 arguments 虽然多数客户端也能吃，但按规范拆开对
// 增量解析型客户端（Claude Code、部分 agent SDK）更稳。
func MakeOpenAIStreamToolCallStartChunk(id, model string, calls []OpenAIToolCall) []byte {
	out := make([]OpenAIToolCall, len(calls))
	for i, tc := range calls {
		idx := i
		tc.Index = &idx
		tc.Function.Arguments = ""
		out[i] = tc
	}
	return marshalChunk(id, model, []OpenAIChoice{
		{Index: 0, Delta: &OpenAIDelta{ToolCalls: out}},
	}, nil)
}

// MakeOpenAIStreamToolCallArgsChunk 参数分片：只带 index 与 arguments 增量。
func MakeOpenAIStreamToolCallArgsChunk(id, model string, index int, args string) []byte {
	idx := index
	return marshalChunk(id, model, []OpenAIChoice{
		{Index: 0, Delta: &OpenAIDelta{ToolCalls: []OpenAIToolCall{
			{Index: &idx, Function: OpenAIToolCallFunc{Arguments: args}},
		}}},
	}, nil)
}

// MakeOpenAIStreamErrorChunk 流中错误对象。
//
// OpenAI 没有官方的「流式错误 chunk」格式，但客户端生态普遍认
// `data: {"error":{"message":…,"type":…}}`（参考实现 mimo2api 的 writeSSEError）。
// 关键是它不能长成 chat.completion.chunk：否则会被当成正常内容吞掉。
func MakeOpenAIStreamErrorChunk(msg string) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
			"type":    "upstream_error",
		},
	})
	return b
}

// MakeOpenAIStreamUsageChunk 独立的 usage 块。
//
// OpenAI 规范：stream_options.include_usage 时最后一个 chunk 携带 usage 且
// choices 为空数组。此前实现复用了收尾块（choices[0].finish_reason="stop"），
// 会在工具调用块之后把 finish_reason 覆盖成 stop，客户端因此认为模型正常结束
// 而不去执行工具调用——直接打断 agent 的工具循环（2026-09 审查发现）。
func MakeOpenAIStreamUsageChunk(id, model string, usage *OpenAIUsage) []byte {
	return marshalChunk(id, model, []OpenAIChoice{}, usage)
}


// MakeOpenAIResponseWithUsage 创建带 usage 的非流式响应
func MakeOpenAIResponseWithUsage(model, content, reasoning string, usage *OpenAIUsage) []byte {
	now := time.Now().Unix()
	fr := "stop"
	resp := OpenAIChatResponse{
		ID:      fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:8]),
		Object:  "chat.completion",
		Created: now,
		Model:   model,
		Choices: []OpenAIChoice{
			{
				Index: 0,
				Message: &OpenAIMessage{
					Role:             "assistant",
					Content:          content,
					ReasoningContent: reasoning,
				},
				FinishReason: &fr,
			},
		},
		Usage: usage,
	}
	data, _ := json.Marshal(resp)
	return data
}

// MakeOpenAIToolCallResponse 创建 OpenAI 非流式工具调用响应。
// content 带上模型在调用工具前的说明文字（OpenAI 规范允许 content 与 tool_calls 并存），
// 此前固定为空串会把模型"我准备读一下文件"这类交代整段丢掉。
func MakeOpenAIToolCallResponse(model, content string, toolCalls []OpenAIToolCall) []byte {
	now := time.Now().Unix()
	fr := "tool_calls"
	resp := OpenAIChatResponse{
		ID:      fmt.Sprintf("chatcmpl-%s", uuid.New().String()[:8]),
		Object:  "chat.completion",
		Created: now,
		Model:   model,
		Choices: []OpenAIChoice{
			{
				Index: 0,
				Message: &OpenAIMessage{
					Role:      "assistant",
					Content:   content,
					ToolCalls: toolCalls,
				},
				FinishReason: &fr,
			},
		},
	}
	data, _ := json.Marshal(resp)
	return data
}
