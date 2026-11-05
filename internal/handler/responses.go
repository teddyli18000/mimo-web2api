package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/teddyli18000/mimo-web2api/internal/config"
	"github.com/teddyli18000/mimo-web2api/internal/pool"
	"github.com/teddyli18000/mimo-web2api/internal/router"
)

// Responses API（POST /v1/responses）—— OpenAI 新端点。
// 第一版只覆盖文本对话：input 支持字符串与消息数组（input_text / text 分片），
// instructions 作为系统指令前置。图片与工具调用仍走 /v1/chat/completions。
type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions"`
	Stream          bool            `json:"stream"`
	MaxOutputTokens int             `json:"max_output_tokens"`
}

type ResponsesHandler struct {
	pool *pool.Pool
}

func NewResponsesHandler(p *pool.Pool) *ResponsesHandler {
	return &ResponsesHandler{pool: p}
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

	query := buildResponsesQuery(req)
	if query == "" {
		writeError(w, http.StatusBadRequest, "input is empty")
		return
	}

	route := router.RouteModel(req.Model, config.Get().DefaultModel)
	client, err := h.pool.Next()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()

	convID := randomHex32()
	var result webResult
	for attempt := 0; attempt < 2; attempt++ {
		result, err = collectWebResult(ctx, client, query, route.Model, convID, "0", nil)
		if err != nil {
			status, msg := upstreamErrorStatus(err.Error())
			writeError(w, status, "mimo error: "+msg)
			return
		}
		if result.UpstreamErr != "" {
			status, msg := upstreamErrorStatus(result.UpstreamErr)
			writeError(w, status, "mimo: "+msg)
			return
		}
		if isWebResultEmpty(result) {
			if attempt == 0 {
				convID = randomHex32() // 新会话重试一次，和 chat 路径一致
				continue
			}
			writeError(w, http.StatusBadGateway, "mimo returned an empty response (after retry)")
			return
		}
		break
	}

	if req.Stream {
		h.stream(w, route.Model, result)
		return
	}
	h.writeJSON(w, route.Model, result)
}

func (h *ResponsesHandler) writeJSON(w http.ResponseWriter, model string, result webResult) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(buildResponsesObject(model, result))
}

// stream 按 Responses API 的事件名输出：response.created → output_text.delta → response.completed。
func (h *ResponsesHandler) stream(w http.ResponseWriter, model string, result webResult) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	id := newResponseID()
	itemID := "msg_" + id[len(id)-12:]

	writeEvent := func(event string, payload interface{}) {
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	}

	writeEvent("response.created", map[string]interface{}{
		"type":     "response.created",
		"response": pendingResponse(model, id),
	})
	if result.Reasoning != "" {
		writeEvent("response.reasoning_summary_text.delta", map[string]interface{}{
			"type":  "response.reasoning_summary_text.delta",
			"delta": result.Reasoning,
		})
	}
	writeEvent("response.output_item.added", map[string]interface{}{
		"type":         "response.output_item.added",
		"output_index": 0,
		"item": map[string]interface{}{
			"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []interface{}{},
		},
	})
	writeEvent("response.output_text.delta", map[string]interface{}{
		"type":          "response.output_text.delta",
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
		"delta":         result.Text,
	})
	writeEvent("response.output_text.done", map[string]interface{}{
		"type":          "response.output_text.done",
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
		"text":          result.Text,
	})
	writeEvent("response.completed", map[string]interface{}{
		"type":     "response.completed",
		"response": buildResponsesObject(model, result),
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func buildResponsesObject(model string, result webResult) map[string]interface{} {
	usage := map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	if result.Usage != nil {
		usage = map[string]int{
			"input_tokens":  result.Usage.PromptTokens,
			"output_tokens": result.Usage.CompletionTokens,
			"total_tokens":  result.Usage.TotalTokens,
		}
	}
	return map[string]interface{}{
		"id":         newResponseID(),
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     "completed",
		"model":      model,
		"output": []interface{}{
			map[string]interface{}{
				"id":     "msg_" + randomHex32()[:12],
				"type":   "message",
				"status": "completed",
				"role":   "assistant",
				"content": []interface{}{
					map[string]interface{}{
						"type":        "output_text",
						"text":        result.Text,
						"annotations": []interface{}{},
					},
				},
			},
		},
		"usage": usage,
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

// buildResponsesQuery 把 Responses 的 input 摊平成上游能吃的单条 query。
// 第一版不保留工具与图片，保持行为可预测；chat/completions 仍是功能完整的入口。
func buildResponsesQuery(req responsesRequest) string {
	var b strings.Builder
	if strings.TrimSpace(req.Instructions) != "" {
		b.WriteString("[System Instruction]\n")
		b.WriteString(strings.TrimSpace(req.Instructions))
		b.WriteString("\n\n")
	}

	raw := strings.TrimSpace(string(req.Input))
	if raw == "" || raw == "null" {
		return strings.TrimSpace(b.String())
	}

	// input 可以是纯字符串
	var asString string
	if err := json.Unmarshal(req.Input, &asString); err == nil {
		b.WriteString(asString)
		return strings.TrimSpace(b.String())
	}

	var items []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return strings.TrimSpace(b.String())
	}
	for _, item := range items {
		text := responsesContentText(item.Content)
		if text == "" {
			continue
		}
		role := item.Role
		if role == "" {
			role = "user"
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
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

