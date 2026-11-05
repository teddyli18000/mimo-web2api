package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
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

type ChatHandler struct {
	pool      *pool.Pool
	convStore *convstore.Store
}

func NewChatHandler(p *pool.Pool, cs *convstore.Store) *ChatHandler {
	return &ChatHandler{pool: p, convStore: cs}
}

// usageData MiMo usage 事件结构
type usageData struct {
	PromptTokens     int          `json:"promptTokens"`
	CompletionTokens int          `json:"completionTokens"`
	TotalTokens      int          `json:"totalTokens"`
	NativeUsage      *nativeUsage `json:"nativeUsage,omitempty"`
}

type nativeUsage struct {
	PromptTokens      int                `json:"prompt_tokens"`
	CompletionTokens  int                `json:"completion_tokens"`
	TotalTokens       int                `json:"total_tokens"`
	PromptDetails     *promptDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionDetails *completionDetails `json:"completion_tokens_details,omitempty"`
}

type promptDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type completionDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}


// sseIdleTimeout 上游 SSE 读流空闲上限：超过这个时间没有任何事件就判定卡死。
// 取值远大于正常出字间隔（首字通常数秒内到达），只用于兜住真正的挂起。
const sseIdleTimeout = 180 * time.Second

// webResult 收集上游 MiMo 网页端 SSE 流的完整结果
type webResult struct {
	Text        string     // 过滤 think 与 \u0000 后的全部正文
	Reasoning   string     // v3 的 reasoningContent（思考链），单独透传给客户端
	Usage       *usageData // 最后一个 usage 事件（fastchat 会发多个，取最后一个）
	LastMsgID   string     // 最后一个带 id 的事件 id
	UpstreamErr string     // event:error 的 content
}

func isWebResultEmpty(r webResult) bool {
	return r.Text == "" && r.UpstreamErr == ""
}

func randomHex32() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// collectWebResult 收集完整结果（非流式路径，以及压缩等内部调用）。
func collectWebResult(ctx context.Context, client *mimo.WebClient, query, model, convID, parentID string, medias []interface{}, thinking bool, reasoningEffort string) (webResult, error) {
	return runWebStream(ctx, client, chatArgs{
		query:    query,
		model:    model,
		convID:   convID,
		parentID: parentID,
		medias:   medias,
		thinking: thinking,
		effort:   reasoningEffort,
	}, streamHandlers{})
}

// collectWebResultFromReader 只有收集需求时的入口（单测直接用内存 reader 造 SSE）。
func collectWebResultFromReader(ctx context.Context, reader io.ReadCloser) (webResult, error) {
	return consumeWebStream(ctx, reader, streamHandlers{})
}

func (h *ChatHandler) Handle(w http.ResponseWriter, r *http.Request) {
	var req adapter.OpenAIChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	routeResult := router.RouteModel(req.Model, config.Get().DefaultModel)
	logging.Set(r.Context(), "model", routeResult.Model)
	logging.Set(r.Context(), "stream", req.Stream)
	log.Printf("[route] model=%s reason=%s", routeResult.Model, routeResult.Reason)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()

	if !h.pool.HasAccounts() {
		writeError(w, http.StatusServiceUnavailable, "no accounts configured")
		return
	}

	h.handleWebChat(ctx, w, &req, routeResult.Model, req.Stream)
}

// handleWebChat 使用网页端反代 — 混合模式：
// query 组装 = 全量重放（system + 全部历史 + 当前消息，参考 meny2333/mimo2api_go 的
// serializeMessages 设计），解决 agent 客户端注入型 user 消息吞掉真实提问的问题；
// 会话连续性 = fingerprint 前缀延续判断（agent 循环追加历史 → 复用 MiMo 服务端上下文；
// 新会话 → 全新 conversationId）。
// 收集策略 = 先完整收集上游响应，检测到空响应自动重试一次（换全新 conversationId）。
func (h *ChatHandler) handleWebChat(ctx context.Context, w http.ResponseWriter, req *adapter.OpenAIChatRequest, model string, stream bool) {
	client, err := h.pool.Next()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	logging.Set(ctx, "account", h.pool.AccountID(client))

	// 组装 (role, text) 序列供 fingerprint 与 serialize 共用
	roleTexts := toRoleTexts(req.Messages)

	// 会话解析：fingerprint 前缀延续 → 复用；否则新建
	convID, parentID, isNew := h.convStore.Resolve(roleTexts)
	if isNew {
		log.Printf("[conv] new conversation %s (model=%s)", convID[:8], model)
	} else {
		log.Printf("[conv] continuing conversation %s (parentID=%s)", convID[:8], parentID[:min(len(parentID), 8)])
	}

	// query 组装策略：
	//   新会话 → 完整上下文（system + 历史 + 当前消息）+ 完整工具 schema
	//   延续会话 → 仅增量消息（上游服务端已有上下文）+ 紧凑工具名提醒
	// 增量发送与网页端原生行为一致：缩短 query、避免长度限制、降低每轮开销。
	//
	// 工具结果轮（本轮没有新的用户提问）必须带上任务锚点：只发工具输出时模型会
	// 丢失任务（2026-09 DSH 实测，模型自述"只收到工具输出、没有原始指令"）。
	budget := router.MaxQueryCharsForModel(model)
	turnQuestion := currentTurnQuestion(req.Messages)
	// 只是应声（"继续"/"好的"）时不覆盖锚点，否则后续工具轮会注入无意义的任务
	if turnQuestion != "" && !(isBareConfirmation(turnQuestion) && h.convStore.Task(convID) != "") {
	}
	taskAnchor := h.convStore.Task(convID)
	// 工具结果轮（本轮没有新提问）需要把任务锚点带回去
	useAnchor := !isNew && turnQuestion == "" && taskAnchor != ""

	var compressedFull string
	buildQuery := func(full bool) string {
		if full && compressedFull != "" {
			return compressedFull
		}
		rts := roleTexts
		if !full {
			rts = toRoleTexts(DeltaMessages(req.Messages))
		}
		var extra []string
		if len(req.Tools) > 0 {
			if full {
				extra = append(extra, buildToolPrompt(req.Tools))
			} else if r := buildToolReminder(req.Tools); r != "" {
				extra = append(extra, r)
			}
		}
		if !full && useAnchor {
			extra = append(extra, "Current task (the user's request you are working on): "+taskAnchor)
		}
		if full {
			rts = compressRoleTexts(ctx, client, rts, model, budget)
		}
		q := serializeRoleTexts(rts, budget, extra...)
		if full {
			compressedFull = q
		}
		return q
	}

	query := buildQuery(isNew)
	if query == "" {
		query = buildQuery(true) // 增量异常时退化为全量
	}
	if query == "" {
		log.Printf("[filter] no valid user message")
		writeError(w, http.StatusBadRequest, "no valid user message")
		return
	}
	anchorNote := ""
	if useAnchor {
		anchorNote = fmt.Sprintf(" anchor=%d", len(taskAnchor))
	}
	log.Printf("[query] len=%d budget=%d mode=%s%s", len(query), budget,
		map[bool]string{true: "full", false: "delta"}[isNew], anchorNote)

	stats.Get().IncrConcurrency()
	defer stats.Get().DecrConcurrency()

	// 图片与文本文件先传到上游资源接口，拿到 id 后随对话一起发（否则模型看不到）
	medias := uploadImages(ctx, client, collectImagesOpenAI(req.Messages))
	medias = append(medias, uploadFiles(ctx, client, collectTextFilesOpenAI(req.Messages))...)

	curConvID := convID
	curParentID := parentID
	replacedFrom := "" // 非空表示空响应重试换过会话，成功后需迁移映射
	var result webResult

	// 流式输出端。响应头懒发：拿到第一段内容之前，错误仍能按正常 HTTP 状态码返回；
	// 一旦写过任何字节（含 keepalive / role 帧），就只能按协议发错误帧了。
	streamID := adapter.NewStreamID()
	var (
		sw    *sseWriter
		guard *toolcall.MarkerGuard
	)
	if stream {
		sw = newSSEWriter(w)
		defer sw.Stop()
		// 真实 OpenAI 流的首帧声明 assistant role，必须早于任何正文
		sw.SetPrelude(openAIFrame(adapter.MakeOpenAIStreamRoleChunk(streamID, model)))
	}

	onReasoning := func(delta string) error {
		if sw == nil {
			return nil
		}
		return sw.Event(adapter.MakeOpenAIStreamReasoningChunk(streamID, model, delta))
	}
	onContent := func(delta string) error {
		if sw == nil {
			return nil
		}
		if guard == nil {
			guard = toolcall.NewMarkerGuard()
		}
		safe, _ := guard.Write(delta)
		if safe == "" {
			return nil
		}
		return sw.Event(adapter.MakeOpenAIStreamContentChunk(streamID, model, safe))
	}

	args := chatArgs{
		medias:   medias,
		thinking: req.ReasoningEffort != "",
		effort:   req.ReasoningEffort,
	}

	for attempt := 0; attempt < 2; attempt++ {
		args.query = query
		args.model = model
		args.convID = curConvID
		args.parentID = curParentID

		var err error
		result, err = runWebStream(ctx, client, args, streamHandlers{
			OnStart: func() {
				if sw != nil {
					go sw.Keepalive(ctx, sseKeepaliveInterval)
				}
			},
			OnReasoning: onReasoning,
			OnContent:   onContent,
		})
		if err != nil {
			reportAccount(h.pool, client, err)
			log.Printf("[error] web chat (attempt %d): %v", attempt+1, err)
			status, msg := upstreamErrorStatus(err.Error())
			// 已经写过字节：状态码回不去了，只能按协议收尾
			if sw != nil && sw.Committed() {
				writeOpenAIStreamError(sw, "mimo error: "+msg)
				return
			}
			abortStream(sw)
			writeError(w, status, "mimo error: "+msg)
			return
		}

		// 能拿到上游事件流就说明账号是活的（业务层面的拒绝不算账号故障）
		h.pool.Report(client, nil)

		// 上游业务错误（文本超长/风控等）一律报错，即使已经吐了部分正文：
		// 把截断内容当成功返回，agent 客户端会据此继续往下走。
		if result.UpstreamErr != "" {
			log.Printf("[error] mimo upstream error (partial len=%d): %s", len(result.Text), result.UpstreamErr)
			status, msg := upstreamErrorStatus(result.UpstreamErr)
			if sw != nil && sw.Committed() {
				writeOpenAIStreamError(sw, "mimo: "+msg)
				return
			}
			abortStream(sw)
			writeError(w, status, "mimo: "+msg)
			return
		}

		// 空响应重试：首次尝试若为空且无明确错误，换全新 conversationId 重试一次。
		// 重试必须改用【完整上下文】——新会话没有服务端历史，只发增量会答非所问。
		// 已经吐过正文就不能重试：客户端会看到重复内容（参考实现 handler.py 同款约束）。
		if isWebResultEmpty(result) {
			if attempt == 0 && (sw == nil || !sw.ContentSent()) {
				log.Printf("[retry] empty response on convID=%s, retrying with full context", curConvID[:min(len(curConvID), 8)])
				replacedFrom = curConvID
				curConvID = randomHex32()
				curParentID = "0"
				query = buildQuery(true)
				continue
			}
			log.Printf("[error] mimo returned an empty response after retry")
			if sw != nil && sw.Committed() {
				writeOpenAIStreamError(sw, "mimo returned an empty response (after retry)")
				return
			}
			abortStream(sw)
			writeError(w, http.StatusBadGateway, "mimo returned an empty response (after retry)")
			return
		}

		break
	}

	// 重试换过会话且最终成功：把会话映射迁到新 convID。
	// 不迁移的话 SetParentID 会落空（新 ID 不在表里），且下一轮请求仍按指纹
	// 命中那个已被判定失效的旧会话，上下文连续性直接断掉。
	if replacedFrom != "" {
		h.convStore.Replace(replacedFrom, curConvID)
	}

	// 记录 usage
	var openaiUsage *adapter.OpenAIUsage
	if u := result.Usage; u != nil {
		cached := 0
		reasoning := 0
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
		log.Printf("[usage] model=%s prompt=%d completion=%d cached=%d reasoning=%d",
			model, u.PromptTokens, u.CompletionTokens, cached, reasoning)
		openaiUsage = &adapter.OpenAIUsage{
			PromptTokens:     u.PromptTokens,
			CompletionTokens: u.CompletionTokens,
			TotalTokens:      u.TotalTokens,
		}
	}

	// 保存对话到 MiMo 官网 + 更新 parentId（仅在有实际内容时）
	if curConvID != "" && result.Text != "" {
		go client.SaveConversation(context.Background(), curConvID, query, model == router.ModelV26UltraSpeed)
	}
	if result.LastMsgID != "" && result.Text != "" {
		h.convStore.SetParentID(curConvID, result.LastMsgID)
		log.Printf("[conv] updated parentId for convID=%s: %s", curConvID[:8], result.LastMsgID[:min(len(result.LastMsgID), 8)])
	}

	// 检测是否包含工具调用。
	// 流式下工具标记已经被 MarkerGuard 扣押，这里只决定结尾怎么收。
	finalText := strings.TrimSpace(result.Text)
	var toolCalls []adapter.OpenAIToolCall
	if toolcall.HasToolCallSyntax(finalText) {
		calls := toolcall.ParseToolCallsFromText(finalText)
		log.Printf("[tools] parsed %d calls from text", len(calls))
		for i, c := range calls {
			log.Printf("[tools] call[%d]: name=%s input=%v", i, c.Name, c.Input)
		}
		if len(calls) > 0 {
			toolCalls = toolcall.ConvertToolCallsToOpenAI(calls)
			log.Printf("[tools] detected %d tool calls in response", len(toolCalls))
		}
	}

	if stream {
		if len(toolCalls) > 0 {
			// 标记之前的说明文字先补发（守卫会连它一起扣住）
			if guard != nil {
				if pre := guard.FlushPrefix(); pre != "" {
					if err := sw.Event(adapter.MakeOpenAIStreamContentChunk(streamID, model, pre)); err != nil {
						return
					}
				}
			}
			// 两阶段下发：先 id/name，再 arguments 分片——与真实 OpenAI 一致，
			// 增量解析型客户端（Claude Code 等）只认这种形状。
			if err := sw.Event(adapter.MakeOpenAIStreamToolCallStartChunk(streamID, model, toolCalls)); err != nil {
				return
			}
			for i, tc := range toolCalls {
				if tc.Function.Arguments == "" {
					continue
				}
				if err := sw.Event(adapter.MakeOpenAIStreamToolCallArgsChunk(streamID, model, i, tc.Function.Arguments)); err != nil {
					return
				}
			}
			if err := sw.Event(adapter.MakeOpenAIStreamFinishReasonChunk(streamID, model, "tool_calls")); err != nil {
				return
			}
		} else {
			// 没解析出工具调用：把守卫扣押的尾巴补发完
			// （含「命中标记但解析失败」的误判场景，不能丢字）
			if guard != nil {
				if rest := guard.Flush(); rest != "" {
					if err := sw.Event(adapter.MakeOpenAIStreamContentChunk(streamID, model, rest)); err != nil {
						return
					}
				}
			}
			if err := sw.Event(adapter.MakeOpenAIStreamFinishChunk(streamID, model)); err != nil {
				return
			}
		}
		if openaiUsage != nil {
			_ = sw.Event(adapter.MakeOpenAIStreamUsageChunk(streamID, model, openaiUsage))
		}
		_ = sw.Raw("data: [DONE]\n\n")
		return
	}

	if len(toolCalls) > 0 {
		respBody := adapter.MakeOpenAIToolCallResponse(model, toolcall.StripToolCallSyntax(finalText), toolCalls)
		if openaiUsage != nil {
			var m map[string]interface{}
			if json.Unmarshal(respBody, &m) == nil {
				m["usage"] = openaiUsage
				respBody, _ = json.Marshal(m)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(respBody)
		return
	}

	respBody := adapter.MakeOpenAIResponseWithUsage(model, result.Text, result.Reasoning, openaiUsage)
	w.Header().Set("Content-Type", "application/json")
	w.Write(respBody)
}

// filterThinkingChunk 状态机方式过滤 thinking 内容
func filterThinkingChunk(content string, inThinking bool) (string, bool) {
	var result strings.Builder

	for len(content) > 0 {
		if inThinking {
			end := strings.Index(content, "</think>")
			if end == -1 {
				return "", true
			}
			content = content[end+8:]
			inThinking = false
			continue
		}

		start := strings.Index(content, "<think>")
		if start == -1 {
			result.WriteString(content)
			break
		}

		result.WriteString(content[:start])
		content = content[start+7:]
		inThinking = true
	}

	return result.String(), inThinking
}


func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
			"type":    "invalid_request_error",
			"code":    status,
		},
	})
}

// upstreamErrorStatus 把上游文案映射成客户端能正确处理的语义：
// 繁忙 → 503（SDK 会退避重试），模型名/内容问题 → 400（重试没有意义），
// 登录态失效 → 502 但文案直接告诉用户去面板重同步 Cookie。
func upstreamErrorStatus(msg string) (int, string) {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(msg, "繁忙") || strings.Contains(msg, "稍后再试") ||
		strings.Contains(lower, "busy") || strings.Contains(lower, "overloaded"):
		return http.StatusServiceUnavailable, msg
	case strings.Contains(msg, "模型名称") || strings.Contains(msg, "模型不存在") ||
		strings.Contains(lower, "model not found") || strings.Contains(lower, "invalid model"):
		return http.StatusBadRequest, "不支持的模型：" + msg
	case strings.Contains(msg, "敏感") || strings.Contains(msg, "违规") ||
		strings.Contains(lower, "sensitive") || strings.Contains(lower, "content policy"):
		return http.StatusBadRequest, "内容被上游拒绝：" + msg
	case strings.Contains(msg, "401") || strings.Contains(msg, "403") ||
		strings.Contains(msg, "未登录") || strings.Contains(msg, "登录"):
		return http.StatusBadGateway, "上游登录态已失效，请在面板重新同步 Cookie：" + msg
	default:
		return http.StatusBadGateway, msg
	}
}

// writeAnthropicError 按 Anthropic 规范返回错误：
//
//	{"type":"error","error":{"type":"invalid_request_error","message":"…"}}
//
// 此前返回 {"error":"…"} 且未设置 Content-Type，官方 SDK 无法取到 err.type
// 与 err.message（2026-09 协议审查发现）。
func writeAnthropicError(w http.ResponseWriter, status int, msg string) {
	errType := "api_error"
	switch {
	case status == http.StatusBadRequest:
		errType = "invalid_request_error"
	case status == http.StatusUnauthorized:
		errType = "authentication_error"
	case status == http.StatusTooManyRequests:
		errType = "rate_limit_error"
	case status == http.StatusServiceUnavailable:
		errType = "overloaded_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": msg,
		},
	})
}



// MessagesHandler Anthropic 格式
type MessagesHandler struct {
	pool      *pool.Pool
	convStore *convstore.Store
}

func NewMessagesHandler(p *pool.Pool, cs *convstore.Store) *MessagesHandler {
	return &MessagesHandler{pool: p, convStore: cs}
}

func (h *MessagesHandler) Handle(w http.ResponseWriter, r *http.Request) {
	var req adapter.AnthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	routeResult := router.RouteModel(req.Model, config.Get().DefaultModel)
	logging.Set(r.Context(), "model", routeResult.Model)
	logging.Set(r.Context(), "stream", req.Stream)
	logging.Set(r.Context(), "protocol", "anthropic")

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()

	if !h.pool.HasAccounts() {
		writeAnthropicError(w, http.StatusServiceUnavailable, "no accounts configured")
		return
	}

	client, err := h.pool.Next()
	if err != nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	logging.Set(ctx, "account", h.pool.AccountID(client))

	// 组装 (role, text) 序列供 fingerprint 与 serialize 共用
	systemText := adapter.NormalizeContent(req.System)
	roleTexts := toRoleTextsAnthropic(req.Messages, systemText)

	// 会话解析：fingerprint 前缀延续 → 复用；否则新建
	convID, parentID, isNew := h.convStore.Resolve(roleTexts)
	if isNew {
		log.Printf("[conv] Anthropic new conversation %s (model=%s)", convID[:8], routeResult.Model)
	} else {
		log.Printf("[conv] Anthropic continuing conversation %s (parentID=%s)", convID[:8], parentID[:min(len(parentID), 8)])
	}

	// query 组装：新会话完整上下文 + 完整工具 schema；延续会话仅增量 + 紧凑工具名提醒。
	// 与 OpenAI 路径一致，工具结果轮要带上任务锚点。
	hasTools := len(req.Tools) > 0
	budgetA := router.MaxQueryCharsForModel(routeResult.Model)
	turnQuestionA := currentTurnQuestionAnthropic(req.Messages)
	// 只是应声（"继续"/"好的"）时不覆盖锚点，否则后续工具轮会注入无意义的任务
	if turnQuestionA != "" && !(isBareConfirmation(turnQuestionA) && h.convStore.Task(convID) != "") {
	}
	taskAnchorA := h.convStore.Task(convID)
	useAnchorA := !isNew && turnQuestionA == "" && taskAnchorA != ""

	var compressedFullA string
	buildQueryA := func(full bool) string {
		if full && compressedFullA != "" {
			return compressedFullA
		}
		rts := roleTexts
		if !full {
			rts = toRoleTextsAnthropic(DeltaMessagesAnthropic(req.Messages), systemText)
		}
		var extra []string
		if hasTools {
			openaiTools := adapter.ConvertAnthropicToolsToOpenAI(req.Tools)
			if full {
				extra = append(extra, buildToolPrompt(openaiTools))
			} else if r := buildToolReminder(openaiTools); r != "" {
				extra = append(extra, r)
			}
		}
		if !full && useAnchorA {
			extra = append(extra, "Current task (the user's request you are working on): "+taskAnchorA)
		}
		if full {
			rts = compressRoleTexts(ctx, client, rts, routeResult.Model, budgetA)
		}
		q := serializeRoleTexts(rts, budgetA, extra...)
		if full {
			compressedFullA = q
		}
		return q
	}
	query := buildQueryA(isNew)
	if query == "" {
		query = buildQueryA(true)
	}
	if query == "" {
		log.Printf("[filter] no valid Anthropic user message")
		writeAnthropicError(w, http.StatusBadRequest, "no valid user message")
		return
	}
	anchorNoteA := ""
	if useAnchorA {
		anchorNoteA = fmt.Sprintf(" anchor=%d", len(taskAnchorA))
	}
	log.Printf("[query] Anthropic len=%d budget=%d mode=%s%s", len(query), budgetA,
		map[bool]string{true: "full", false: "delta"}[isNew], anchorNoteA)

	stats.Get().IncrConcurrency()
	defer stats.Get().DecrConcurrency()

	mediasA := uploadImages(ctx, client, collectImagesAnthropic(req.Messages))
	mediasA = append(mediasA, uploadFiles(ctx, client, collectTextFilesAnthropic(req.Messages))...)

	// Anthropic 用 thinking.budget_tokens 表达强度，映射成上游的档位
	thinkingA := req.Thinking != nil && req.Thinking.Type != "disabled"
	effortA := ""
	if thinkingA {
		effortA = "high"
		if b := req.Thinking.BudgetTokens; b > 0 && b < 8000 {
			effortA = "medium"
		}
	}

	curConvID := convID
	curParentID := parentID
	replacedFrom := "" // 非空表示空响应重试换过会话，成功后需迁移映射
	var result webResult

	// 流式状态：内容块索引懒分配。
	// Anthropic 要求 message_start 先于所有 content_block，且块顺序不可回退，
	// 所以 thinking 块先开（index 0），正文到来时先关它再开 text 块（index 1）。
	var (
		sw       *sseWriter
		guard    *toolcall.MarkerGuard
		msgID    = fmt.Sprintf("msg_%s", uuid.New().String()[:24])
		nextIdx  int
		thinkIdx = -1
		textIdx  = -1
		thinkOpn bool
		textOpn  bool
		// messageStarted 单独记：keepalive 可能已经写过响应头，
		// 但 message_start 事件必须且只能发一次。
		messageStarted bool
	)
	if req.Stream {
		sw = newSSEWriter(w)
		defer sw.Stop()
	}

	startMessage := func() error {
		if sw == nil || messageStarted {
			return nil
		}
		messageStarted = true
		// input_tokens 此刻还不知道（上游 usage 在流末尾），先发 0；
		// message_delta 会补 output_tokens，与参考实现 meny2333 一致。
		return sw.EventName("message_start", adapter.MakeAnthropicStreamEvent("message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"model":         routeResult.Model,
				"content":       []interface{}{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]interface{}{"input_tokens": 0, "output_tokens": 0},
			},
		}))
	}

	openBlock := func(kind string, initial map[string]interface{}) (int, error) {
		idx := nextIdx
		nextIdx++
		block := map[string]interface{}{"type": kind}
		for k, v := range initial {
			block[k] = v
		}
		err := sw.EventName("content_block_start", adapter.MakeAnthropicStreamEvent("content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         idx,
			"content_block": block,
		}))
		return idx, err
	}

	closeBlock := func(idx int) error {
		return sw.EventName("content_block_stop", adapter.MakeAnthropicStreamEvent("content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": idx,
		}))
	}

	ensureText := func() error {
		if textOpn {
			return nil
		}
		if thinkOpn {
			if err := closeBlock(thinkIdx); err != nil {
				return err
			}
			thinkOpn = false
		}
		idx, err := openBlock("text", map[string]interface{}{"text": ""})
		if err != nil {
			return err
		}
		textIdx, textOpn = idx, true
		return nil
	}

	writeText := func(delta string) error {
		if delta == "" {
			return nil
		}
		if err := startMessage(); err != nil {
			return err
		}
		if err := ensureText(); err != nil {
			return err
		}
		return sw.EventName("content_block_delta", adapter.MakeAnthropicStreamEvent("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": textIdx,
			"delta": map[string]interface{}{"type": "text_delta", "text": delta},
		}))
	}

	onReasoning := func(delta string) error {
		if sw == nil {
			return nil
		}
		if err := startMessage(); err != nil {
			return err
		}
		if thinkIdx < 0 {
			idx, err := openBlock("thinking", map[string]interface{}{"thinking": "", "signature": ""})
			if err != nil {
				return err
			}
			thinkIdx, thinkOpn = idx, true
		}
		return sw.EventName("content_block_delta", adapter.MakeAnthropicStreamEvent("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": thinkIdx,
			"delta": map[string]interface{}{"type": "thinking_delta", "thinking": delta},
		}))
	}

	onContent := func(delta string) error {
		if sw == nil {
			return nil
		}
		if guard == nil {
			guard = toolcall.NewMarkerGuard()
		}
		safe, _ := guard.Write(delta)
		return writeText(safe)
	}

	argsA := chatArgs{
		model:    routeResult.Model,
		medias:   mediasA,
		thinking: thinkingA,
		effort:   effortA,
	}

	for attempt := 0; attempt < 2; attempt++ {
		argsA.query = query
		argsA.convID = curConvID
		argsA.parentID = curParentID

		var err error
		result, err = runWebStream(ctx, client, argsA, streamHandlers{
			OnStart: func() {
				if sw != nil {
					go sw.Keepalive(ctx, sseKeepaliveInterval)
				}
			},
			OnReasoning: onReasoning,
			OnContent:   onContent,
		})
		if err != nil {
			reportAccount(h.pool, client, err)
			log.Printf("[error] Anthropic web chat (attempt %d): %v", attempt+1, err)
			status, msg := upstreamErrorStatus(err.Error())
			if sw != nil && sw.Committed() {
				writeAnthropicStreamError(sw, msg)
				return
			}
			abortStream(sw)
			writeAnthropicError(w, status, msg)
			return
		}

		// 能拿到上游事件流就说明账号是活的（业务层面的拒绝不算账号故障）
		h.pool.Report(client, nil)

		// 上游业务错误一律报错，即使已有部分正文（同 OpenAI 路径）
		if result.UpstreamErr != "" {
			log.Printf("[error] Anthropic mimo upstream error (partial len=%d): %s", len(result.Text), result.UpstreamErr)
			status, msg := upstreamErrorStatus(result.UpstreamErr)
			if sw != nil && sw.Committed() {
				writeAnthropicStreamError(sw, "mimo: "+msg)
				return
			}
			abortStream(sw)
			writeAnthropicError(w, status, "mimo: "+msg)
			return
		}

		// 空响应重试：首次尝试若为空且无明确错误，换全新 conversationId 重试一次。
		// 已经吐过正文就不能重试（客户端会看到重复内容）。
		if isWebResultEmpty(result) {
			if attempt == 0 && (sw == nil || !sw.ContentSent()) {
				log.Printf("[retry] Anthropic empty response on convID=%s, retrying with full context", curConvID[:min(len(curConvID), 8)])
				replacedFrom = curConvID
				curConvID = randomHex32()
				curParentID = "0"
				query = buildQueryA(true) // 与 OpenAI 路径一致：新会话必须带完整上下文
				continue
			}
			log.Printf("[error] Anthropic mimo returned an empty response after retry")
			if sw != nil && sw.Committed() {
				writeAnthropicStreamError(sw, "mimo returned an empty response (after retry)")
				return
			}
			abortStream(sw)
			writeAnthropicError(w, http.StatusBadGateway, "mimo returned an empty response (after retry)")
			return
		}

		break
	}

	if replacedFrom != "" {
		h.convStore.Replace(replacedFrom, curConvID)
	}

	// 记录 usage
	inTokens, outTokens := 0, 0
	if u := result.Usage; u != nil {
		cached := 0
		reasoning := 0
		if u.NativeUsage != nil {
			if u.NativeUsage.PromptDetails != nil {
				cached = u.NativeUsage.PromptDetails.CachedTokens
			}
			if u.NativeUsage.CompletionDetails != nil {
				reasoning = u.NativeUsage.CompletionDetails.ReasoningTokens
			}
		}
		stats.Get().Record(routeResult.Model, u.PromptTokens, u.CompletionTokens, cached, reasoning, u.TotalTokens)
		logging.Set(ctx, "prompt_tokens", u.PromptTokens)
		logging.Set(ctx, "completion_tokens", u.CompletionTokens)
		inTokens = u.PromptTokens
		outTokens = u.CompletionTokens
	}

	// 保存对话到 MiMo 官网历史记录 + 更新 parentId（仅在有实际内容时）
	if curConvID != "" && result.Text != "" {
		go client.SaveConversation(context.Background(), curConvID, query, routeResult.Model == router.ModelV26UltraSpeed)
	}
	if result.LastMsgID != "" && result.Text != "" {
		h.convStore.SetParentID(curConvID, result.LastMsgID)
		log.Printf("[conv] Anthropic: updated parentId for convID=%s: %s", curConvID[:8], result.LastMsgID[:min(len(result.LastMsgID), 8)])
	}

	finalText := strings.TrimSpace(result.Text)
	var calls []toolcall.ParsedToolCall
	if hasTools && toolcall.HasToolCallSyntax(finalText) {
		calls = toolcall.ParseToolCallsFromText(finalText)
		log.Printf("[tools] Anthropic: parsed %d calls (raw len=%d): %q",
			len(calls), len(finalText), finalText[:min(len(finalText), 400)])
	}

	if req.Stream {
		if err := startMessage(); err != nil {
			return
		}

		if len(calls) > 0 {
			// 标记前的说明文字先补发（守卫会连它一起扣住）
			if guard != nil {
				if pre := guard.FlushPrefix(); pre != "" {
					if err := writeText(pre); err != nil {
						return
					}
				}
			}
			// 关掉还开着的块，工具块从下一个索引继续
			if thinkOpn {
				if err := closeBlock(thinkIdx); err != nil {
					return
				}
				thinkOpn = false
			}
			if textOpn {
				if err := closeBlock(textIdx); err != nil {
					return
				}
				textOpn = false
			}
			for _, c := range calls {
				block := adapter.AnthropicToolUseBlock{
					Type:  "tool_use",
					ID:    "toolu_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:24],
					Name:  c.Name,
					Input: c.Input,
				}
				idx := nextIdx
				nextIdx++
				if err := sw.EventName("content_block_start", adapter.MakeAnthropicStreamEvent("content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]interface{}{
						"type":  "tool_use",
						"id":    block.ID,
						"name":  block.Name,
						"input": map[string]interface{}{},
					},
				})); err != nil {
					return
				}
				// 参数按规范走 input_json_delta：只在 start 里塞完整 input 时，
				// 按增量解析的客户端会拿到空参数。
				if raw, err := json.Marshal(block.Input); err == nil {
					if err := sw.EventName("content_block_delta", adapter.MakeAnthropicStreamEvent("content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": idx,
						"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": string(raw)},
					})); err != nil {
						return
					}
				}
				if err := closeBlock(idx); err != nil {
					return
				}
			}
			_ = sw.EventName("message_delta", adapter.MakeAnthropicStreamEvent("message_delta", map[string]interface{}{
				"type": "message_delta",
				"delta": map[string]interface{}{
					"stop_reason":   "tool_use",
					"stop_sequence": nil,
				},
				"usage": map[string]interface{}{"input_tokens": inTokens, "output_tokens": outTokens},
			}))
			_ = sw.EventName("message_stop", adapter.MakeAnthropicStreamEvent("message_stop", nil))
			return
		}

		// 普通正文：把守卫扣押的尾巴补发完
		if guard != nil {
			if rest := guard.Flush(); rest != "" {
				if err := writeText(rest); err != nil {
					return
				}
			}
		}
		// 至少给一个 text 块：即使一个字都没有，content 数组也得有形状
		if err := ensureText(); err != nil {
			return
		}
		if thinkOpn {
			if err := closeBlock(thinkIdx); err != nil {
				return
			}
			thinkOpn = false
		}
		if textOpn {
			if err := closeBlock(textIdx); err != nil {
				return
			}
			textOpn = false
		}
		_ = sw.EventName("message_delta", adapter.MakeAnthropicStreamEvent("message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason":   "end_turn",
				"stop_sequence": nil,
			},
			"usage": map[string]interface{}{"input_tokens": inTokens, "output_tokens": outTokens},
		}))
		_ = sw.EventName("message_stop", adapter.MakeAnthropicStreamEvent("message_stop", nil))
		return
	}

	if len(calls) > 0 {
		blocks := make([]interface{}, 0, len(calls))
		for _, c := range calls {
			blocks = append(blocks, adapter.AnthropicToolUseBlock{
				Type:  "tool_use",
				ID:    "toolu_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:24],
				Name:  c.Name,
				Input: c.Input,
			})
		}
		resp := map[string]interface{}{
			"id":          fmt.Sprintf("msg_%s", uuid.New().String()[:24]),
			"type":        "message",
			"role":        "assistant",
			"content":     blocks,
			"model":       routeResult.Model,
			"stop_reason": "tool_use",
			"usage":       map[string]interface{}{"input_tokens": inTokens, "output_tokens": outTokens},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	blocks := make([]interface{}, 0, 2)
	if thinkingA && result.Reasoning != "" {
		blocks = append(blocks, map[string]interface{}{
			"type": "thinking", "thinking": result.Reasoning, "signature": "",
		})
	}
	blocks = append(blocks, map[string]interface{}{"type": "text", "text": finalText})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":            fmt.Sprintf("msg_%s", uuid.New().String()[:24]),
		"type":          "message",
		"role":          "assistant",
		"model":         routeResult.Model,
		"content":       blocks,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         map[string]interface{}{"input_tokens": inTokens, "output_tokens": outTokens},
	})
}



