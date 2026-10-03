package mimo

import (
	"net/url"
	"strings"
	"testing"
)

// TestNormalizeCredential 从 DevTools 复制 Cookie 时经常连外层引号一起粘进来。
// ph 带引号会让上游 bot/chat 返回 401，而健康检查（只发 Cookie）仍然是绿的，
// 表现为"面板显示健康但发不出消息"。构造时必须清洗掉。
func TestNormalizeCredential(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"abc=="`, "abc=="},
		{`abc==`, "abc=="},
		{`  "abc=="  `, "abc=="},
		{`"abc=="`, "abc=="},
		{``, ""},
		{`""`, ""},
		{`"a"b"`, `a"b`}, // 只削掉首尾，内部引号保留
	}
	for _, c := range cases {
		if got := normalizeCredential(c.in); got != c.want {
			t.Errorf("normalizeCredential(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestQuotedCredentialsAreCleanedEverywhere 带引号的凭证在 Cookie 与 URL 两处
// 都必须被清洗——之前只在 Cookie 里 trim，URL 用了原始值，于是 401。
func TestQuotedCredentialsAreCleanedEverywhere(t *testing.T) {
	c := NewWebClient(`"tok=="`, `"12345"`, `"ph=="`)

	cookie := c.buildCookie()
	if strings.Contains(cookie, `"tok==""`) || strings.Contains(cookie, `\"\"`) {
		t.Errorf("Cookie 里残留了成对引号: %s", cookie)
	}
	if !strings.Contains(cookie, "userId=12345") {
		t.Errorf("userId 未被清洗: %s", cookie)
	}

	// URL 查询串：这正是之前漏掉的地方
	chatURL := "https://example.invalid/open-apis/bot/chat?xiaomichatbot_ph=" + url.QueryEscape(c.ph)
	if strings.Contains(chatURL, "%22") {
		t.Errorf("URL 里残留引号（上游会返回 401 serviceLogin）: %s", chatURL)
	}
	if !strings.Contains(chatURL, "xiaomichatbot_ph=ph%3D%3D") {
		t.Errorf("URL 里的 ph 不正确: %s", chatURL)
	}
}
