package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

type ctxKey int

const reqStateKey ctxKey = iota

// reqState 是请求级字段袋。
// handler 在关键节点 Set 模型/账号/流式/token，访问日志统一落盘，
// 这样一条请求在日志里只有一行，而不是散落的十几条。
type reqState struct {
	id     string
	mu     sync.Mutex
	fields map[string]interface{}
}

// RequestID 给每个请求生成 ID，写响应头 X-Request-ID 并放进 context。
//
// 客户端报错时只要给出这个 ID，就能在日志里定位整条链路。
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		st := &reqState{id: id, fields: map[string]interface{}{"req_id": id}}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), reqStateKey, st)))
	})
}

func newRequestID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// IDFrom 取当前请求 ID（不在请求上下文里时返回空串）。
func IDFrom(ctx context.Context) string {
	if st, ok := ctx.Value(reqStateKey).(*reqState); ok {
		return st.id
	}
	return ""
}

// Set 给当前请求附加字段。没有请求上下文时静默忽略。
func Set(ctx context.Context, key string, value interface{}) {
	st, ok := ctx.Value(reqStateKey).(*reqState)
	if !ok {
		return
	}
	st.mu.Lock()
	st.fields[key] = value
	st.mu.Unlock()
}

// AccessLog 在请求结束时输出一条访问日志。
//
// 必须包在 RequestID 之后：它依赖 context 里的字段袋。
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)

		fields := map[string]interface{}{
			"method": r.Method,
			"path":   r.URL.Path,
			"status": rw.status,
			"dur_ms": time.Since(start).Milliseconds(),
			"bytes":  rw.bytes,
		}
		if st, ok := r.Context().Value(reqStateKey).(*reqState); ok {
			st.mu.Lock()
			for k, v := range st.fields {
				fields[k] = v
			}
			st.mu.Unlock()
		}

		level := InfoLevel
		if rw.status >= 500 {
			level = ErrorLevel
		}
		Log(level, "http_request", fields)
	})
}

// responseWriter 记录状态码与字节数。
//
// Flush 必须透传：SSE 的真流式全靠它，包一层不带 Flush 的 ResponseWriter
// 会把流式重新变回「攒够再发」。
type responseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (w *responseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *responseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能拿到底层 ResponseWriter。
func (w *responseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
