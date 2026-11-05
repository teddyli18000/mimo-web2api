package mimo

import (
	"errors"
	"fmt"
	"testing"
)

func TestHTTPErrorKeepsLegacyText(t *testing.T) {
	err := &HTTPError{Status: 401, Body: `{"code":401}`}
	// upstreamErrorStatus 靠字符串里的 "401" 做语义映射，文本格式不能变
	want := `mimo returned 401: {"code":401}`
	if err.Error() != want {
		t.Fatalf("Error()=%q want %q", err.Error(), want)
	}
}

func TestIsAuthError(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{401, true},
		{403, true},
		{451, true},
		{429, false},
		{500, false},
		{404, false},
	}
	for _, c := range cases {
		err := &HTTPError{Status: c.status}
		if got := IsAuthError(err); got != c.want {
			t.Fatalf("IsAuthError(%d)=%v want %v", c.status, got, c.want)
		}
	}

	if IsAuthError(errors.New("mimo returned 401: x")) {
		t.Fatal("普通 error 不应被当成认证错误（必须走 HTTPError）")
	}
	if IsAuthError(nil) {
		t.Fatal("nil 不是认证错误")
	}

	wrapped := fmt.Errorf("call failed: %w", &HTTPError{Status: 403})
	if !IsAuthError(wrapped) {
		t.Fatal("包装后的认证错误应能被识别")
	}
}
