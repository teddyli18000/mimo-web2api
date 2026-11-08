package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/teddyli18000/mimo-web2api/internal/adapter"
	"github.com/teddyli18000/mimo-web2api/internal/config"
	"github.com/teddyli18000/mimo-web2api/internal/convstore"
	"github.com/teddyli18000/mimo-web2api/internal/logging"
	"github.com/teddyli18000/mimo-web2api/internal/mimo"
	"github.com/teddyli18000/mimo-web2api/internal/pool"
	"github.com/teddyli18000/mimo-web2api/internal/router"
	"github.com/teddyli18000/mimo-web2api/internal/stats"
	"github.com/teddyli18000/mimo-web2api/internal/toolcall"
)

// responsesTool 是 Responses API 的 function tool（字段平铺，不是
// chat/completions 里 function:{...} 的嵌套形状）。
type responsesTool struct {
	Type        string      `json:"type"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions"`
	Stream          bool            `json:"stream"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Tools           []responsesTool `json:"tools"`
}

// responseCall 是 Responses 协议里的 function_call 输出项。
// call_id 供客户端回传 function_call_output 时配对，id 是这个 item 自己的 ID。
type responseCall struct {
	ID        string
	CallID    string
	Name      string
	Arguments string
}

type ResponsesHandler struct {
	pool      *pool.Pool
	convStore *convstore.Store
}

func NewResponsesHandler(p *pool.Pool, cs *convstore.Store) *ResponsesHandler {
	return &ResponsesHandler{pool: p, convStore: cs}
}

func (h *ResponsesHandler) Handle(w http.ResponseWriter, r *http.Request) {
	var req responsesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !h.pool.HasAccounts() {
		writeError(w, http.StatusServiceUnavailable, "no accounts configured")
		return
	}

	route := router.RouteModel(req.Model, config.Get().DefaultModel)
	logging.Set(r.Context(), "model", route.Model)
	logging.Set(r.Context(), "stream", req.Stream)
	logging.Set(r.Context(), "protocol", "responses")

	rts := responsesRoleTexts(req)
	if len(rts) == 0 {
		writeError(w, http.StatusBadRequest, "input is empty")
		return
	}

	openaiTools := responsesToOpenAITools(req.Tools)
	toolPrompt := buildToolPrompt(openaiTools)
	budget := router.MaxQueryCharsForModel(route.Model)

	convID, parentID, isNew := h.convStore.Resolve(rts)
	queryRts := rts
	if !isNew {
		queryRts = deltaRoleTexts(rts)
	}
	query := serializeQuery(queryRts, budget, toolPrompt)
	// 重问时重发完整上下文；纠正提示走 extraSystem，和工具契约一起计入预算，
	// 不能简单往序列化结果后面拼字符串（超预算时会把 query 顶过上游硬上限）。
	fullQuery := func(correction string) string {
		if correction == "" {
			return serializeQuery(rts, budget, toolPrompt)
		}
		return serializeQuery(rts, budget, toolPrompt, correction)
	}
	if query == "" {
		writeError(w, http.StatusBadRequest, "no valid user message")
		return
	}

	client, err := h.pool.Next()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	logging.Set(r.Context(), "account", h.pool.AccountID(client))

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()

	stats.Get().IncrConcurrency()
	defer stats.Get().DecrConcurrency()

	args := chatArgs{
		query:    query,
		model:    route.Model,
		convID:   convID,
		parentID: parentID,
	}

	if req.Stream {
		h.handleStream(ctx, w, client, route.Model, args, fullQuery, openaiTools)
		return
	}

	replacedFrom := ""
	var result webResult
	for attempt := 0; attempt < 2; attempt++ {
		result, err = runWebStream(ctx, client, args, streamHandlers{})
		if err != nil {
			reportAccount(h.pool, client, err)
			status, msg := upstreamErrorStatus(err.Error())
			writeError(w, status, "mimo error: "+msg)
			return
		}
		h.pool.Report(client, nil)
		if result.UpstreamErr != "" {
			status, msg := upstreamErrorStatus(result.UpstreamErr)
			writeError(w, status, "mimo: "+msg)
			return
		}
		if isWebResultEmpty(result) {
			if attempt == 0 {
				replacedFrom = args.convID
				args.convID = randomHex32()
				args.parentID = "0"
				args.query = fullQuery("")
				continue
			}
			writeError(w, http.StatusBadGateway, "mimo returned an empty response (after retry)")
			return
		}

		// 同 chat 路径：工具调用漏必填参数时带纠正提示重问一次
		if len(openaiTools) > 0 && attempt == 0 {
			calls := normalizeToolNames(toolcall.ParseToolCallsFromText(strings.TrimSpace(result.Text)), openaiTools)
			if missing := toolcall.MissingRequired(calls, openaiTools); len(missing) > 0 {
				log.Printf("[retry] responses: incomplete tool call, missing %v", missing)
				replacedFrom = args.convID
				args.convID = randomHex32()
				args.parentID = "0"
				args.query = fullQuery(toolcall.CorrectionNote(missing))
				continue
			}
		}
		break
	}
	if replacedFrom != "" {
		h.convStore.Replace(replacedFrom, args.convID)
	}

	finalText := strings.TrimSpace(result.Text)
	calls := parseResponseCalls(finalText, openaiTools)
	h.recordUsage(ctx, result, route.Model)
	h.afterStream(ctx, client, args.convID, args.query, route.Model, result)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(buildResponsesObject(route.Model, result, calls, newResponseID()))
}

// handleStream 边收边发：文本走 output_text.delta，工具调用在流尾补成
// function_call 项。response.completed 必须与 response.created 共用同一个 id。
func (h *ResponsesHandler) handleStream(ctx context.Context, w http.ResponseWriter, client *mimo.WebClient, model string, args chatArgs, fullQuery func(string) string, tools []adapter.OpenAITool) {
	sw := newSSEWriter(w)
	defer sw.Stop()

	respID := newResponseID()
	msgID := "msg_" + randomHex32()[:12]
	var (
		guard       *toolcall.MarkerGuard
		messageOpen bool
		textBuf     strings.Builder
	)

	emit := func(name string, payload map[string]interface{}) error {
		payload["type"] = name
		data, _ := json.Marshal(payload)
		return sw.EventName(name, data)
	}

	if err := emit("response.created", map[string]interface{}{
		"response": pendingResponse(model, respID),
	}); err != nil {
		return
	}

	openMessage := func() error {
		if messageOpen {
			return nil
		}
		messageOpen = true
		if err := emit("response.output_item.added", map[string]interface{}{
			"output_index": 0,
			"item": map[string]interface{}{
				"id": msgID, "type": "message", "status": "in_progress",
				"role": "assistant", "content": []interface{}{},
			},
		}); err != nil {
			return err
		}
		return emit("response.content_part.added", map[string]interface{}{
			"item_id": msgID, "output_index": 0, "content_index": 0,
			"part": map[string]interface{}{
				"type": "output_text", "text": "", "annotations": []interface{}{},
			},
		})
	}

	writeText := func(delta string) error {
		if delta == "" {
			return nil
		}
		if err := openMessage(); err != nil {
			return err
		}
		textBuf.WriteString(delta)
		return emit("response.output_text.delta", map[string]interface{}{
			"item_id": msgID, "output_index": 0, "content_index": 0, "delta": delta,
		})
	}

	var result webResult
	var runErr error
	replacedFrom := ""
	// 纠正重试时第一轮可能已经吐过 reasoning，第二轮不能重复下发事件。
	silentCorrection := false
	correctionSilent := false
	for attempt := 0; attempt < 2; attempt++ {
		handlers := streamHandlers{
			OnStart: func() { go sw.Keepalive(ctx, sseKeepaliveInterval) },
			OnReasoning: func(delta string) error {
				return emit("response.reasoning_summary_text.delta", map[string]interface{}{"delta": delta})
			},
			OnContent: func(delta string) error {
				if guard == nil {
					guard = toolcall.NewMarkerGuard()
				}
				safe, _ := guard.Write(delta)
				return writeText(safe)
			},
		}
		correctionSilent = silentCorrection
		if silentCorrection {
			handlers = streamHandlers{}
		}
		result, runErr = runWebStream(ctx, client, args, handlers)
		if runErr != nil {
			break
		}
		if result.UpstreamErr != "" {
			break
		}
		if isWebResultEmpty(result) && attempt == 0 && !messageOpen {
			replacedFrom = args.convID
			args.convID = randomHex32()
			args.parentID = "0"
			args.query = fullQuery("") // 新会话必须重发完整上下文
			continue
		}
		// 工具调用漏必填参数：还没写过消息正文时带纠正提示重问一次
		if len(tools) > 0 && attempt == 0 && !messageOpen {
			calls := normalizeToolNames(toolcall.ParseToolCallsFromText(strings.TrimSpace(result.Text)), tools)
			if missing := toolcall.MissingRequired(calls, tools); len(missing) > 0 {
				log.Printf("[retry] responses stream: incomplete tool call, missing %v", missing)
				guard = nil
				silentCorrection = true
				replacedFrom = args.convID
				args.convID = randomHex32()
				args.parentID = "0"
				args.query = fullQuery(toolcall.CorrectionNote(missing))
				continue
			}
		}
		break
	}

	// response.created 已经写出，状态码回不去了：错误只能按协议发事件。
	fail := func(msg string) {
		_ = emit("error", map[string]interface{}{
			"message": msg, "type": "upstream_error", "code": "upstream_error",
		})
		_ = emit("response.failed", map[string]interface{}{
			"response": map[string]interface{}{
				"id": respID, "object": "response", "status": "failed", "model": model,
				"error": map[string]interface{}{"code": "upstream_error", "message": msg},
			},
		})
	}
	if runErr != nil {
		reportAccount(h.pool, client, runErr)
		fail(runErr.Error())
		return
	}
	h.pool.Report(client, nil)

	if result.UpstreamErr != "" {
		fail(result.UpstreamErr)
		return
	}
	if replacedFrom != "" {
		h.convStore.Replace(replacedFrom, args.convID)
	}

	finalText := strings.TrimSpace(result.Text)
	calls := parseResponseCalls(finalText, tools)

	if len(calls) == 0 && guard != nil {
		if rest := guard.Flush(); rest != "" {
			if err := writeText(rest); err != nil {
				return
			}
		}
	} else if len(calls) == 0 && correctionSilent {
		// 静默纠正那一轮没有任何增量下发；模型若改成纯文字回答，
		// 必须在这里把整段正文补出去，否则客户端只收到空 message。
		if err := writeText(finalText); err != nil {
			return
		}
	}
	if !messageOpen && len(calls) == 0 {
		if err := openMessage(); err != nil {
			return
		}
	}

	if messageOpen {
		text := textBuf.String()
		part := map[string]interface{}{"type": "output_text", "text": text, "annotations": []interface{}{}}
		_ = emit("response.output_text.done", map[string]interface{}{
			"item_id": msgID, "output_index": 0, "content_index": 0, "text": text,
		})
		_ = emit("response.content_part.done", map[string]interface{}{
			"item_id": msgID, "output_index": 0, "content_index": 0, "part": part,
		})
		_ = emit("response.output_item.done", map[string]interface{}{
			"output_index": 0,
			"item": map[string]interface{}{
				"id": msgID, "type": "message", "status": "completed", "role": "assistant",
				"content": []interface{}{part},
			},
		})
	}

	// 工具项排在 message 之后，output_index 从 1 开始
	for i, c := range calls {
		outIdx := i + 1
		item := map[string]interface{}{
			"id": c.ID, "type": "function_call", "status": "in_progress",
			"call_id": c.CallID, "name": c.Name, "arguments": "",
		}
		if err := emit("response.output_item.added", map[string]interface{}{
			"output_index": outIdx, "item": item,
		}); err != nil {
			return
		}
		if err := emit("response.function_call_arguments.delta", map[string]interface{}{
			"item_id": c.ID, "output_index": outIdx, "delta": c.Arguments,
		}); err != nil {
			return
		}
		if err := emit("response.function_call_arguments.done", map[string]interface{}{
			"item_id": c.ID, "output_index": outIdx, "arguments": c.Arguments,
		}); err != nil {
			return
		}
		item["status"] = "completed"
		item["arguments"] = c.Arguments
		if err := emit("response.output_item.done", map[string]interface{}{
			"output_index": outIdx, "item": item,
		}); err != nil {
			return
		}
	}

	h.recordUsage(ctx, result, model)
	h.afterStream(ctx, client, args.convID, args.query, model, result)

	_ = emit("response.completed", map[string]interface{}{
		"response": buildResponsesObject(model, result, calls, respID),
	})
}

func (h *ResponsesHandler) recordUsage(ctx context.Context, result webResult, model string) {
	if result.Usage == nil {
		return
	}
	u := result.Usage
	cached, reasoning := 0, 0
	if u.NativeUsage != nil {
		if u.NativeUsage.PromptDetails != nil {
			cached = u.NativeUsage.PromptDetails.CachedTokens
		}
		if u.NativeUsage.CompletionDetails != nil {
			reasoning = u.NativeUsage.CompletionDetails.ReasoningTokens
		}
	}
	stats.Get().Record(model, u.PromptTokens, u.CompletionTokens, cached, reasoning, u.TotalTokens)
	logging.Set(ctx, "prompt_tokens", u.PromptTokens)
	logging.Set(ctx, "completion_tokens", u.CompletionTokens)
}

func (h *ResponsesHandler) afterStream(ctx context.Context, client *mimo.WebClient, convID, query, model string, result webResult) {
	if convID != "" && result.Text != "" {
		go client.SaveConversation(context.Background(), convID, query, model == router.ModelV26UltraSpeed)
	}
	if result.LastMsgID != "" && result.Text != "" {
		h.convStore.SetParentID(convID, result.LastMsgID)
	}
}

func parseResponseCalls(text string, tools []adapter.OpenAITool) []responseCall {
	if len(tools) == 0 || !toolcall.HasToolCallSyntax(text) {
		return nil
	}
	calls := normalizeToolNames(toolcall.ParseToolCallsFromText(text), tools)
	if len(calls) == 0 {
		return nil
	}
	out := make([]responseCall, 0, len(calls))
	for _, c := range calls {
		args, _ := json.Marshal(c.Input)
		out = append(out, responseCall{
			ID:        "fc_" + randomHex32()[:24],
			CallID:    "call_" + randomHex32()[:24],
			Name:      c.Name,
			Arguments: string(args),
		})
	}
	log.Printf("[tools] responses: parsed %d calls", len(out))
	return out
}

func responsesToOpenAITools(tools []responsesTool) []adapter.OpenAITool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]adapter.OpenAITool, 0, len(tools))
	for _, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		out = append(out, adapter.OpenAITool{
			Type: "function",
			Function: adapter.OpenAIToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return out
}

// responsesRoleTexts 把 Responses 的 input 摊平成 (role, text) 序列。
//
// 除了普通消息，还要认 function_call / function_call_output 两种 item：
// 客户端回传工具结果时用的是它们，没有 role/content，旧实现会直接跳过，
// 于是模型永远看不到自己刚发起的调用结果。
func responsesRoleTexts(req responsesRequest) []roleText {
	var out []roleText
	if s := strings.TrimSpace(req.Instructions); s != "" {
		out = append(out, roleText{"system", s})
	}

	raw := strings.TrimSpace(string(req.Input))
	if raw == "" || raw == "null" {
		return out
	}

	var asString string
	if err := json.Unmarshal(req.Input, &asString); err == nil {
		if asString != "" {
			out = append(out, roleText{"user", asString})
		}
		return out
	}

	var items []struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Output    json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return out
	}

	for _, item := range items {
		switch item.Type {
		case "function_call":
			out = append(out, roleText{"assistant", fmt.Sprintf("[Tool Calls]\n%s(%s)", item.Name, item.Arguments)})
		case "function_call_output":
			ref := ""
			if item.CallID != "" {
				ref = " (" + item.CallID + ")"
			}
			text := responsesContentText(item.Output)
			if text == "" {
				var s string
				if json.Unmarshal(item.Output, &s) == nil {
					text = s
				}
			}
			if text != "" {
				out = append(out, roleText{"tool", "[Tool Result]" + ref + ":\n" + text})
			}
		default:
			text := responsesContentText(item.Content)
			if text == "" {
				continue
			}
			role := item.Role
			if role == "" {
				role = "user"
			}
			out = append(out, roleText{role, text})
		}
	}
	return out
}

// deltaRoleTexts 延续会话时只发最后一个 assistant 之后的消息，
// system（instructions）保留：上游服务端上下文不一定还完整留着它。
func deltaRoleTexts(rts []roleText) []roleText {
	last := -1
	for i := len(rts) - 1; i >= 0; i-- {
		if rts[i][0] == "assistant" {
			last = i
			break
		}
	}
	if last < 0 || last+1 >= len(rts) {
		return rts
	}
	out := make([]roleText, 0, len(rts)-last)
	for _, rt := range rts {
		if rt[0] == "system" {
			out = append(out, rt)
		}
	}
	out = append(out, rts[last+1:]...)
	return out
}

func responsesContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// buildResponsesObject 组装响应体；id 由调用方给，保证流式事件里
// response.created 与 response.completed 指向同一个 response。
func buildResponsesObject(model string, result webResult, calls []responseCall, id string) map[string]interface{} {
	usage := map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	if result.Usage != nil {
		usage = map[string]int{
			"input_tokens":  result.Usage.PromptTokens,
			"output_tokens": result.Usage.CompletionTokens,
			"total_tokens":  result.Usage.TotalTokens,
		}
	}

	text := result.Text
	if len(calls) > 0 {
		text = toolcall.StripToolCallSyntax(strings.TrimSpace(text))
	}
	output := make([]interface{}, 0, len(calls)+1)
	if text != "" || len(calls) == 0 {
		output = append(output, map[string]interface{}{
			"id":     "msg_" + randomHex32()[:12],
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []interface{}{
				map[string]interface{}{
					"type":        "output_text",
					"text":        text,
					"annotations": []interface{}{},
				},
			},
		})
	}
	for _, c := range calls {
		output = append(output, map[string]interface{}{
			"id":        c.ID,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   c.CallID,
			"name":      c.Name,
			"arguments": c.Arguments,
		})
	}

	return map[string]interface{}{
		"id":         id,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     "completed",
		"model":      model,
		"output":     output,
		"usage":      usage,
	}
}

func pendingResponse(model, id string) map[string]interface{} {
	return map[string]interface{}{
		"id":         id,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     "in_progress",
		"model":      model,
		"output":     []interface{}{},
	}
}

func newResponseID() string {
	return "resp_" + randomHex32()
}
