package handler

import (
	"net/http"
	"testing"
)

func TestUpstreamErrorStatus(t *testing.T) {
	cases := []struct {
		msg  string
		want int
	}{
		{"平台繁忙，请稍后再试", http.StatusServiceUnavailable},
		{"upstream busy", http.StatusServiceUnavailable},
		{"overloaded", http.StatusServiceUnavailable},
		{"模型名称不存在", http.StatusBadRequest},
		{"invalid model: mimo-x", http.StatusBadRequest},
		{"内容包含敏感信息", http.StatusBadRequest},
		{"sensitive content detected", http.StatusBadRequest},
		{"mimo returned 401: unauthorized", http.StatusBadGateway},
		{"mimo returned 403: forbidden", http.StatusBadGateway},
		{"用户未登录", http.StatusBadGateway},
		{"mimo returned 500: boom", http.StatusBadGateway},
		{"", http.StatusBadGateway},
	}

	for _, c := range cases {
		got, msg := upstreamErrorStatus(c.msg)
		if got != c.want {
			t.Errorf("upstreamErrorStatus(%q) = %d, want %d", c.msg, got, c.want)
		}
		if c.msg != "" && msg == "" {
			t.Errorf("upstreamErrorStatus(%q) returned an empty message", c.msg)
		}
	}
}
