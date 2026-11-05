package handler

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func sseFrame(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// 一次读取同时满足两个需求：增量回调（给客户端）与完整结果（落库/统计）。
func TestConsumeWebStreamEmitsIncrementalDeltas(t *testing.T) {
	payload := strings.Join([]string{
		sseFrame("dialogId", `{"content":"dlg_1"}`),
		sseFrame("message", `{"reasoningContent":"想法"}`),
		sseFrame("message", `{"content":"第一段"}`),
		sseFrame("message", `{"content":"第二段"}`),
		sseFrame("usage", `{"promptTokens":3,"completionTokens":4,"totalTokens":7}`),
		sseFrame("finish", `{"content":"[DONE]"}`),
	}, "")

	var reasoning, content []string
	res, err := consumeWebStream(context.Background(), io.NopCloser(strings.NewReader(payload)), streamHandlers{
		OnReasoning: func(d string) error { reasoning = append(reasoning, d); return nil },
		OnContent:   func(d string) error { content = append(content, d); return nil },
	})
	if err != nil {
		t.Fatalf("consumeWebStream: %v", err)
	}

	if len(reasoning) != 1 || reasoning[0] != "想法" {
		t.Fatalf("思考链增量不对: %#v", reasoning)
	}
	if len(content) != 2 || content[0] != "第一段" || content[1] != "第二段" {
		t.Fatalf("正文应逐段回调，实际: %#v", content)
	}
	if res.Text != "第一段第二段" {
		t.Fatalf("完整正文不对: %q", res.Text)
	}
	if res.Reasoning != "想法" {
		t.Fatalf("完整思考链不对: %q", res.Reasoning)
	}
	if res.Usage == nil || res.Usage.PromptTokens != 3 || res.Usage.TotalTokens != 7 {
		t.Fatalf("usage 没收集到: %+v", res.Usage)
	}
	if res.LastMsgID != "dlg_1" {
		t.Fatalf("dialogId 没收集到: %q", res.LastMsgID)
	}
}

// 客户端已经断开时，回调返回错误要立刻停止读上游，并带上 errStreamClosed 标记，
// 让账号池知道这不是上游/账号的问题。
func TestConsumeWebStreamStopsWhenClientGone(t *testing.T) {
	payload := sseFrame("message", `{"content":"第一段"}`) + sseFrame("message", `{"content":"第二段"}`)

	_, err := consumeWebStream(context.Background(), io.NopCloser(strings.NewReader(payload)), streamHandlers{
		OnContent: func(string) error { return errors.New("broken pipe") },
	})
	if !errors.Is(err, errStreamClosed) {
		t.Fatalf("应返回 errStreamClosed，实际: %v", err)
	}
}

// 上游会把 webSearch 之类的状态文案混进 message 流，它不是模型输出。
func TestConsumeWebStreamDropsUpstreamNoise(t *testing.T) {
	payload := sseFrame("message", `{"type":"text","content":"webSearch"}`) +
		sseFrame("message", `{"type":"text","content":"正文"}`) +
		sseFrame("finish", `{"content":"[DONE]"}`)

	res, err := consumeWebStream(context.Background(), io.NopCloser(strings.NewReader(payload)), streamHandlers{})
	if err != nil {
		t.Fatalf("consumeWebStream: %v", err)
	}
	if res.Text != "正文" {
		t.Fatalf("状态文案应被丢弃，实际: %q", res.Text)
	}
}

// 空结果（只有 dialogId/usage/finish）要能被上层识别，触发空响应重试。
func TestConsumeWebStreamEmptyResult(t *testing.T) {
	payload := sseFrame("dialogId", `{"content":"dlg_empty"}`) +
		sseFrame("usage", `{"promptTokens":1,"completionTokens":0,"totalTokens":1}`) +
		sseFrame("finish", `{"content":"[DONE]"}`)

	res, err := consumeWebStream(context.Background(), io.NopCloser(strings.NewReader(payload)), streamHandlers{})
	if err != nil {
		t.Fatalf("consumeWebStream: %v", err)
	}
	if !isWebResultEmpty(res) {
		t.Fatalf("应判定为空响应: %+v", res)
	}
}
