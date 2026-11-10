package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/teddyli18000/mimo-web2api/internal/adapter"
	"github.com/teddyli18000/mimo-web2api/internal/mimo"
	"github.com/teddyli18000/mimo-web2api/internal/pool"
	"github.com/teddyli18000/mimo-web2api/internal/toolcall"
)

// cleanStreamTail 清掉流式收尾补发的文本里的工具标记残片。
//
// 真机实测：模型会吐一个没有任何 invoke 的空 `<tool_calls>\n</tool_calls>`；
// MarkerGuard 把它扣住，但解析结果为 0 个调用时走「误判补发」分支，
// 于是内部标记被当成正文发给客户端。
//
// 只在确实含标记时才清洗：普通正文（哪怕提到 "tool_calls" 这个词）原样返回，
// 避免把用户可见的文字删掉。
func cleanStreamTail(text string) string {
	if text == "" || !toolcall.HasToolCallSyntax(text) {
		return text
	}
	return toolcall.StripToolCallSyntax(text)
}

// sseKeepaliveInterval 等首字节/两个增量之间的心跳间隔。
//
// 上游网页端 SSE 在长思考时可能几十秒不吐事件，客户端（pi-ai、Claude Code）
// 自己有 stream idle 超时（实测 300s），中间层（nginx、Cloudflare）也会掐空闲
// 连接。真流式下我们不能再像缓冲模式那样「安静地等」，必须定期写注释帧。
const sseKeepaliveInterval = 15 * time.Second

// errStreamClosed 表示客户端连接已经写不动了（断连/被取消）。
// 这类错误不是上游的问题，不能用来给账号记失败。
var errStreamClosed = errors.New("stream closed by client")

// chatArgs 一次上游对话请求的参数集合。
// 原来 collectWebResult 有 9 个位置参数，真流式还要再带回调，抽出结构体避免调用点失控。
type chatArgs struct {
	query    string
	model    string
	convID   string
	parentID string
	medias   []interface{}
	thinking bool
	effort   string
}

// streamHandlers 消费上游增量。
//
// 三个回调都可能为 nil：全 nil 就是原来的「收集模式」。
// 回调返回错误表示客户端已经断开，应立即停止读流。
type streamHandlers struct {
	// OnStart 在拿到上游响应头之后、读第一条事件之前调用。
	// 用于启动 keepalive：此时 HTTP 层已经确定成功，可以安全地提前写 200。
	OnStart func()
	// OnReasoning 思考链增量（v3 的 reasoningContent）。
	OnReasoning func(delta string) error
	// OnContent 正文增量（已过滤 think 标记与 \u0000）。
	OnContent func(delta string) error
}

// runWebStream 发起一次上游对话并把增量交给 handlers，同时返回完整结果。
// 重试/空响应判定都在调用方，这里只负责一次请求 + 一次读流。
func runWebStream(ctx context.Context, client *mimo.WebClient, a chatArgs, h streamHandlers) (webResult, error) {
	body, err := client.Chat(ctx, a.query, a.model, a.convID, a.parentID, a.thinking, a.medias, a.effort)
	if err != nil {
		return webResult{}, err
	}
	if h.OnStart != nil {
		h.OnStart()
	}
	return consumeWebStream(ctx, body, h)
}

// consumeWebStream 解析上游网页端 SSE。
//
// 与旧的 collectWebResultFromReader 相比多了两个增量回调：正文/思考链每到一个
// message 事件就回调一次，同时仍然累积完整结果（统计、落库、工具解析都要用）。
func consumeWebStream(ctx context.Context, reader io.ReadCloser, h streamHandlers) (webResult, error) {
	// 可取消：读流空闲超时需要主动中断底层的 SSE 解析
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	events := make(chan mimo.WebSSEEvent, 64)
	errChan := make(chan error, 1)
	go func() {
		defer close(events)
		errChan <- mimo.ParseWebSSE(ctx, reader, events)
	}()

	var (
		content     strings.Builder
		reasoning   strings.Builder
		inThinking  bool
		lastUsage   *usageData
		lastMsgID   string
		upstreamErr string
	)

	// 读流空闲超时：上游可能建连后长时间不吐数据，没有这个上界时整个请求会一直
	// 挂到客户端自己的超时（实测出现过 pi-ai "stream idle timeout after 300000ms"）。
	idle := time.NewTimer(sseIdleTimeout)
	defer idle.Stop()

	// emit 把增量交给调用方；失败说明客户端已经走了，没必要继续读上游。
	// 用 errStreamClosed 包一层：写失败不是上游/账号的问题，不能计入账号失败。
	emit := func(fn func(string) error, delta string) error {
		if fn == nil || delta == "" {
			return nil
		}
		if err := fn(delta); err != nil {
			return fmt.Errorf("%w: %v", errStreamClosed, err)
		}
		return nil
	}

collect:
	for {
		select {
		case <-idle.C:
			cancel()
			return webResult{}, fmt.Errorf("upstream stalled: no SSE event for %v", sseIdleTimeout)
		case <-ctx.Done():
			return webResult{}, ctx.Err()
		case ev, ok := <-events:
			if !ok {
				break collect
			}
			idle.Reset(sseIdleTimeout)
			switch ev.Event {
			case "dialogId":
				// v3：这一轮响应的 dialogId，下一轮要作为 previousDialogueId 回传
				var d struct {
					Content string `json:"content"`
				}
				if err := json.Unmarshal([]byte(ev.Data), &d); err == nil && d.Content != "" {
					lastMsgID = d.Content
				}
			case "message":
				if ev.ID != "" {
					lastMsgID = ev.ID
				}
				var msg struct {
					Type             string `json:"type"`
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoningContent"`
				}
				if err := json.Unmarshal([]byte(ev.Data), &msg); err != nil {
					continue
				}
				// v3 的思考链单独放在 reasoningContent：不混进正文，但原样记下来，
				// 后面以 reasoning_content 字段透传给客户端（DeepSeek 系客户端认这个）。
				if msg.ReasoningContent != "" {
					reasoning.WriteString(msg.ReasoningContent)
					if err := emit(h.OnReasoning, msg.ReasoningContent); err != nil {
						cancel()
						return webResult{}, err
					}
				}
				// v2 是 type=text；v3 不再带 type，正文在 content。
				if msg.Content != "" && (msg.Type == "" || msg.Type == "text") {
					// 上游会混进 webSearch 之类的状态文案，它不是模型输出。
					// （参考实现 mimo2api：type=="text" 且 content=="webSearch"/"[DONE]" 直接丢）
					if upstreamNoise(msg.Content) {
						continue
					}
					c := strings.ReplaceAll(msg.Content, "\u0000", "")
					c, inThinking = filterThinkingChunk(c, inThinking)
					if c != "" {
						content.WriteString(c)
						if err := emit(h.OnContent, c); err != nil {
							cancel()
							return webResult{}, err
						}
					}
				}
			case "error":
				var e struct {
					Type    string `json:"type"`
					Content string `json:"content"`
					Message string `json:"message"`
				}
				if err := json.Unmarshal([]byte(ev.Data), &e); err == nil {
					if e.Content != "" {
						upstreamErr = e.Content
					} else if e.Message != "" {
						upstreamErr = e.Message
					}
				}
			case "finish":
				// v3 用 finish 事件 + data {"content":"[DONE]"} 收尾，不必等连接关闭
				var f struct {
					Content string `json:"content"`
				}
				if err := json.Unmarshal([]byte(ev.Data), &f); err == nil && f.Content == "[DONE]" {
					break collect
				}
			case "usage":
				var u usageData
				if err := json.Unmarshal([]byte(ev.Data), &u); err == nil {
					lastUsage = &u // 覆盖：以最后一个 usage 为准（fastchat 多 usage 事件规避）
				}
			}
		}
	}

	if err := <-errChan; err != nil && ctx.Err() != nil {
		return webResult{}, err
	}

	return webResult{
		Text:        content.String(),
		Reasoning:   reasoning.String(),
		Usage:       lastUsage,
		LastMsgID:   lastMsgID,
		UpstreamErr: upstreamErr,
	}, nil
}

// upstreamNoise 上游正文流里的控制/状态文案，不属于模型输出。
func upstreamNoise(s string) bool {
	switch strings.TrimSpace(s) {
	case "webSearch", "web_search", "[DONE]":
		return true
	}
	return false
}

// sseWriter 面向客户端的 SSE 写端。
//
// 为什么不用 bufio：真流式的价值全在「上游出一段就立刻到客户端」，
// 任何攒批都会把首字延迟拉回到整段生成时间。
// 所有写入都带 flush，并用互斥锁保护 keepalive 协程与主流程的并发写。
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher

	mu      sync.Mutex
	started bool // 响应头已写（可能是 keepalive 触发的）
	content bool // 已经写出过正文/思考/工具等真实内容
	closed  bool

	// keepaliveOn 保证心跳协程只有一个：重试时 OnStart 会被再调一次。
	keepaliveOn atomic.Bool

	// prelude 在第一帧真实内容之前补发的原始帧（OpenAI 的 role 帧）。
	// keepalive 不走这里：它只是注释，客户端不会把它当内容。
	prelude     []byte
	preludeDone bool
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	f, _ := w.(http.Flusher)
	return &sseWriter{w: w, f: f}
}

func (s *sseWriter) startLocked() {
	if s.started {
		return
	}
	s.started = true
	hdr := s.w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	// 关掉 nginx 的代理缓冲，否则流会被中间层攒成一坨
	hdr.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.flushLocked()
}

// SetPrelude 设置第一帧内容前补发的原始帧（OpenAI 的 role 帧）。
// 必须在任何写操作之前调用。
func (s *sseWriter) SetPrelude(frame []byte) {
	s.mu.Lock()
	s.prelude = frame
	s.mu.Unlock()
}

// writePreludeLocked 补发 prelude；已持锁。
func (s *sseWriter) writePreludeLocked() error {
	if s.preludeDone || len(s.prelude) == 0 {
		return nil
	}
	s.preludeDone = true
	return s.writeLocked(s.prelude)
}

func (s *sseWriter) flushLocked() {
	if s.f != nil {
		s.f.Flush()
	}
}

func (s *sseWriter) writeLocked(b []byte) error {
	if s.closed {
		return errStreamClosed
	}
	if _, err := s.w.Write(b); err != nil {
		s.closed = true
		return err
	}
	s.flushLocked()
	return nil
}

// Event 写一帧 OpenAI 风格 `data: <json>`。
func (s *sseWriter) Event(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startLocked()
	if err := s.writePreludeLocked(); err != nil {
		return err
	}
	if err := s.writeLocked(openAIFrame(data)); err != nil {
		return err
	}
	s.content = true
	return nil
}

// openAIFrame 把 JSON 包成 OpenAI 的 data 帧。
func openAIFrame(data []byte) []byte {
	buf := make([]byte, 0, len(data)+8)
	buf = append(buf, "data: "...)
	buf = append(buf, data...)
	buf = append(buf, "\n\n"...)
	return buf
}

// EventName 写一帧 Anthropic 风格 `event: <name>` + `data: <json>`。
func (s *sseWriter) EventName(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startLocked()
	if err := s.writePreludeLocked(); err != nil {
		return err
	}
	buf := make([]byte, 0, len(name)+len(data)+16)
	buf = append(buf, "event: "...)
	buf = append(buf, name...)
	buf = append(buf, "\ndata: "...)
	buf = append(buf, data...)
	buf = append(buf, "\n\n"...)
	if err := s.writeLocked(buf); err != nil {
		return err
	}
	s.content = true
	return nil
}

// Raw 写原始帧（[DONE]、注释心跳等），不计入 content。
func (s *sseWriter) Raw(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startLocked()
	return s.writeLocked([]byte(text))
}

// ContentSent 是否已经写出过真实内容。
// 空响应重试只允许在 false 时进行：一旦吐过字，重试就会让客户端看到重复内容。
func (s *sseWriter) ContentSent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.content
}

// Committed 是否已经写出过任何字节（含 keepalive 与 role 帧）。
// true 之后不能再写 HTTP 状态码，只能按协议发错误帧。
func (s *sseWriter) Committed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// Keepalive 周期性写 SSE 注释帧，直到 ctx 结束或写失败。
// 多次调用只有一个协程生效（重试会让 OnStart 再触发一次）。
func (s *sseWriter) Keepalive(ctx context.Context, d time.Duration) {
	if !s.keepaliveOn.CompareAndSwap(false, true) {
		return
	}
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				return
			}
			s.startLocked()
			_ = s.writeLocked([]byte(": keepalive\n\n"))
			s.mu.Unlock()
		}
	}
}

// Stop 标记流结束，让 keepalive 协程尽快退出。
func (s *sseWriter) Stop() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// abortStream 在「还没向客户端写出任何字节」时收尾。
//
// 必须显式停掉心跳：keepalive 协程可能正等在 ticker 上，
// 一旦它在 writeError 之后写响应头，就会变成 superfluous WriteHeader + 脏 body。
func abortStream(sw *sseWriter) {
	if sw != nil {
		sw.Stop()
	}
}

// reportAccount 把一次请求结果上报给账号池。
// 客户端写失败（errStreamClosed）不算账号问题，直接跳过。
func reportAccount(p *pool.Pool, client *mimo.WebClient, err error) {
	if errors.Is(err, errStreamClosed) {
		return
	}
	p.Report(client, err)
}

// writeOpenAIStreamError 已提交后（已经发过 200 与内容）的上游错误。
//
// 状态码回不去了，只能按 OpenAI 流式客户端能识别的形状发错误对象再收尾。
// 不发 [DONE] 会让部分客户端把断流当成可重试错误、继续打到同一个坏账号上。
func writeOpenAIStreamError(sw *sseWriter, msg string) {
	if sw == nil {
		return
	}
	_ = sw.Event(adapter.MakeOpenAIStreamErrorChunk(msg))
	_ = sw.Raw("data: [DONE]\n\n")
}

// writeAnthropicStreamError 已提交后的上游错误：Anthropic 用 event: error 收尾，
// 且不再补 message_stop（官方 SDK 见到 error 就结束本轮）。
func writeAnthropicStreamError(sw *sseWriter, msg string) {
	if sw == nil {
		return
	}
	_ = sw.EventName("error", adapter.MakeAnthropicStreamEvent("error", map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    "api_error",
			"message": msg,
		},
	}))
}
