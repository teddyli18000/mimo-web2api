package handler

import (
	"context"
	"log"
	"strings"

	"github.com/teddyli18000/mimo-web2api/internal/mimo"
)

// 上下文压缩：query 接近上游长度上限时，先让模型把历史压成摘要，
// 再拼上最近一段原文。纯截断会丢掉前面所有关键约束（文件路径、待办、数字），
// 摘要能保住这些，代价是长会话多一次上游调用。
const (
	compressTrigger    = 0.8  // 超过预算 80% 触发
	compressKeepRatio  = 0.3  // 摘要后保留尾部原文的比例
	compressPromptChars = 12000 // 送进摘要请求的历史上限
)

func compressQuery(ctx context.Context, client *mimo.WebClient, query, model string, budget int) string {
	if budget <= 0 || float64(len(query)) < float64(budget)*compressTrigger {
		return query
	}

	keep := int(float64(budget) * compressKeepRatio)
	if keep <= 0 || keep > len(query) {
		keep = len(query) / 3
	}
	head := query[:len(query)-keep]
	tail := query[len(query)-keep:]
	if len(head) > compressPromptChars {
		head = head[len(head)-compressPromptChars:]
	}

	prompt := "请把下面的对话历史压缩成不超过 600 字的事实摘要：" +
		"保留结论、数字、文件路径、待办与用户约束；不要编造，不要加评论。\n\n" + head

	result, err := collectWebResult(ctx, client, prompt, model, randomHex32(), "0", nil, false, "")
	if err != nil || strings.TrimSpace(result.Text) == "" {
		log.Printf("[compress] skipped (err=%v, summary len=%d), fallback to truncation", err, len(result.Text))
		return query
	}

	summary := "[历史摘要]\n" + strings.TrimSpace(result.Text) + "\n\n" + tail
	log.Printf("[compress] %d -> %d chars", len(query), len(summary))
	return summary
}
