package mimo

import (
	"errors"
	"fmt"
)

// HTTPError 上游 HTTP 层失败（非 2xx）。
//
// 单独成型是为了让账号池能按状态码判断「是不是这个账号废了」：
// 之前所有失败都被压成一段字符串，只能靠 strings.Contains("401") 猜，
// 一旦上游把数字写进别的错误文案就会误判。
type HTTPError struct {
	Status int
	Body   string
}

// Error 保持与旧实现完全一致：
// upstreamErrorStatus 靠 "401"/"403"/"繁忙" 等子串做语义映射，文本不能变。
func (e *HTTPError) Error() string {
	return fmt.Sprintf("mimo returned %d: %s", e.Status, e.Body)
}

// IsAuthError 是否是账号层面的登录态失效。
//
// 只有这类错误值得立刻把账号摘除：换个账号就能继续服务。
// 429/5xx 是通道繁忙，账号本身没问题，不该计入账号失败。
func IsAuthError(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		switch he.Status {
		case 401, 403, 451:
			return true
		}
	}
	return false
}
