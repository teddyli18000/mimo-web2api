package handler

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Anthropic 的 count_tokens 端点。上游没有对应接口，这里做本地估算：
// CJK 约 1 字 1 token，其余按 4 字符 1 token。客户端只用它判断上下文是否超限，
// 不需要精确到 tiktoken 级别，也不值得为此引一个分词库。
type countTokensRequest struct {
	Model    string      `json:"model"`
	System   interface{} `json:"system"`
	Messages []struct {
		Content interface{} `json:"content"`
	} `json:"messages"`
	Tools interface{} `json:"tools"`
}

func CountTokensHandler(w http.ResponseWriter, r *http.Request) {
	var req countTokensRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	var b strings.Builder
	b.WriteString(collectText(req.System))
	for _, m := range req.Messages {
		b.WriteString(collectText(m.Content))
	}
	if req.Tools != nil {
		if raw, err := json.Marshal(req.Tools); err == nil {
			b.Write(raw)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": EstimateTokens(b.String())})
}

// collectText 处理 Anthropic 的 content 两种形态：纯字符串与 [{type:text,text}]。
func collectText(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []interface{}:
		var b strings.Builder
		for _, item := range t {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if s, ok := m["text"].(string); ok {
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		return ""
	}
}

func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range s {
		if r >= 0x2E80 { // CJK 及全角符号
			cjk++
		} else {
			other++
		}
	}
	n := cjk + (other+3)/4
	if n < 1 {
		n = 1
	}
	return n
}
