package handler

import (
	"context"
	"log"
	"strings"

	"github.com/teddyli18000/mimo-web2api/internal/mimo"
)

// 上下文压缩：原始历史超过预算 80% 时，先让上游把较早的部分压成摘要，
// 再拼上最近几轮原文。纯截断会把历史整段丢掉（serializeRoleTexts 超限时
// 正是这么做的），摘要能保住文件路径、数字、待办这些关键约束。
const (
	compressTrigger     = 0.8   // 触发阈值（占预算比例）
	compressKeepMsgs    = 4     // 摘要后保留的最近消息数
	compressPromptChars = 12000 // 送给摘要请求的历史上限
)

func compressRoleTexts(ctx context.Context, client *mimo.WebClient, rts []roleText, model string, budget int) []roleText {
	if budget <= 0 {
		return rts
	}

	total := 0
	for _, rt := range rts {
		total += len(rt[1])
	}
	if float64(total) < float64(budget)*compressTrigger {
		return rts
	}

	var system, rest []roleText
	for _, rt := range rts {
		if rt[0] == "system" {
			system = append(system, rt)
		} else {
			rest = append(rest, rt)
		}
	}
	if len(rest) <= compressKeepMsgs {
		return rts // 全是最近消息，没有可压缩的历史
	}

	cut := len(rest) - compressKeepMsgs
	// 摘要请求也是发给同一个上游的，历史里的身份指纹同样要清洗；
	// 否则「主请求洗干净了、摘要请求却把原文送出去」，风控仍会命中。
	older := cleanRoleTextsForUpstream(rest[:cut])
	recent := rest[cut:]

	var b strings.Builder
	for _, rt := range older {
		b.WriteString(rt[0])
		b.WriteString(": ")
		b.WriteString(rt[1])
		b.WriteString("\n")
	}
	history := b.String()
	if len(history) > compressPromptChars {
		history = history[len(history)-compressPromptChars:]
	}

	prompt := "请把下面的对话历史压缩成不超过 600 字的事实摘要：" +
		"保留结论、数字、文件路径、待办与用户约束；不要编造，不要加评论。\n\n" + history

	result, err := collectWebResult(ctx, client, prompt, model, randomHex32(), "0", nil, false, "")
	if err != nil || strings.TrimSpace(result.Text) == "" {
		log.Printf("[compress] skipped (err=%v, summary len=%d), fallback to truncation", err, len(result.Text))
		return rts
	}

	out := make([]roleText, 0, len(system)+1+len(recent))
	out = append(out, system...)
	out = append(out, mkRT("system", "[历史摘要]\n"+strings.TrimSpace(result.Text)))
	out = append(out, recent...)
	log.Printf("[compress] %d -> %d chars (%d older messages summarized)", total, len(result.Text), len(older))
	return out
}
