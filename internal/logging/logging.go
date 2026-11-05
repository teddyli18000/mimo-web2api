// Package logging 是网关的结构化日志。
//
// 设计目标（按重要性排序）：
//  1. 不拖慢请求：写入走有界队列 + 单独协程，队列满就丢并计数，绝不阻塞。
//  2. 能排查：每行一条 JSON，带时间、级别、请求 ID 与任意字段；按天切分。
//  3. 不泄漏：serviceToken / ph / user_id 这类凭据永远不落盘。
//  4. 不堆积：超过保留期（默认 7 天）的文件自动删除，运行中定期清理。
//
// 兼容存量调用点：log.SetOutput(logging.Writer()) 之后，仓库里上百处
// log.Printf 不用改，适配器按前缀推断级别。
package logging

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Level 日志级别。
type Level int

const (
	DebugLevel Level = iota
	InfoLevel
	WarnLevel
	ErrorLevel
)

func (l Level) String() string {
	switch l {
	case DebugLevel:
		return "debug"
	case WarnLevel:
		return "warn"
	case ErrorLevel:
		return "error"
	default:
		return "info"
	}
}

// ParseLevel 解析 LOG_LEVEL，非法值回落 info。
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return DebugLevel
	case "warn", "warning":
		return WarnLevel
	case "error":
		return ErrorLevel
	default:
		return InfoLevel
	}
}

// Config 初始化参数。
type Config struct {
	// Dir 日志目录（通常是 <baseDir>/data/logs）。
	Dir string
	// Level 最低输出级别，默认 info。
	Level Level
	// Retention 保留天数，默认 7 天。
	Retention time.Duration
	// Console 是否同时输出到 stderr，默认 true。
	Console bool
	// QueueSize 异步队列长度，默认 4096。
	QueueSize int
}

const (
	// flushInterval 最长多久落盘一次。日志的价值在于事后排查，
	// 200ms 的延迟对排查无感，但能把高频小写入合并成顺序 IO。
	flushInterval = 200 * time.Millisecond
	// flushThreshold 攒够这么多字节立即落盘。
	flushThreshold = 32 * 1024
	defaultQueue   = 4096
	defaultKeep    = 7 * 24 * time.Hour
)

// Logger 是日志实例。包级单例由 Init 安装。
type Logger struct {
	cfg Config
	ch  chan []byte
	// dropped 队列满导致丢弃的行数；只增不减，启动时打印一次。
	dropped atomic.Uint64
	closed  atomic.Bool

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	mu      sync.Mutex
	file    *os.File
	bw      *bufio.Writer
	curDate string
}

var std atomic.Pointer[Logger]

// Init 打开日志目录并启动写入协程。重复调用会先关闭旧实例。
func Init(cfg Config) error {
	if cfg.Dir == "" {
		return fmt.Errorf("logging: empty dir")
	}
	if cfg.Retention <= 0 {
		cfg.Retention = defaultKeep
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return err
	}

	l := &Logger{
		cfg:  cfg,
		ch:   make(chan []byte, cfg.QueueSize),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	// 先切一次文件：目录不可写时要能在启动日志里喊出来
	l.mu.Lock()
	err := l.rotateLocked(time.Now())
	l.mu.Unlock()
	if err != nil {
		return err
	}

	if prev := std.Swap(l); prev != nil {
		prev.Shutdown()
	}
	go l.run()
	go l.cleanupLoop()
	return nil
}

// Shutdown 停止写入协程并把队列里剩下的日志落盘。
func Shutdown() {
	l := std.Load()
	if l == nil {
		return
	}
	l.Shutdown()
}

func (l *Logger) Shutdown() {
	l.stopOnce.Do(func() {
		l.closed.Store(true)
		// 未经过 Init 的实例（单测直接构造）没有协程可停
		if l.stop == nil {
			return
		}
		close(l.stop)
		<-l.done
	})
}

// ---- 记录 ----

// Log 写一条日志。fields 可以为 nil。
func Log(level Level, msg string, fields map[string]interface{}) {
	l := std.Load()
	if l == nil {
		// 未初始化（例如单测直接调 handler）：退回 stderr，保持可见
		fmt.Fprintf(os.Stderr, "%s %s %s\n", time.Now().Format("15:04:05.000"), level.String(), msg)
		return
	}
	if level < l.cfg.Level {
		return
	}

	rec := make(map[string]interface{}, len(fields)+4)
	for k, v := range fields {
		rec[k] = v
	}
	rec["ts"] = time.Now().Format("2006-01-02T15:04:05.000Z07:00")
	rec["level"] = level.String()
	rec["msg"] = msg

	line, err := json.Marshal(rec)
	if err != nil {
		line = []byte(fmt.Sprintf(`{"ts":%q,"level":"error","msg":"log marshal failed"}`, time.Now().Format(time.RFC3339)))
	}
	line = append(line, '\n')

	if l.cfg.Console {
		fmt.Fprintln(os.Stderr, l.consoleLine(level, msg, fields))
	}

	if l.closed.Load() {
		return
	}
	select {
	case l.ch <- line:
	default:
		// 队列满：宁可丢日志也不能卡住请求
		l.dropped.Add(1)
	}
}

func (l *Logger) consoleLine(level Level, msg string, fields map[string]interface{}) string {
	var b strings.Builder
	b.WriteString(time.Now().Format("15:04:05.000"))
	b.WriteByte(' ')
	b.WriteString(strings.ToUpper(level.String()))
	if id, _ := fields["req_id"].(string); id != "" {
		b.WriteString(" [")
		b.WriteString(id)
		b.WriteString("]")
	}
	b.WriteByte(' ')
	b.WriteString(msg)
	// 请求 ID 已经单独显示，其余字段追加在末尾
	for k, v := range fields {
		if k == "req_id" {
			continue
		}
		b.WriteByte(' ')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(fmt.Sprint(v))
	}
	return b.String()
}

// Debugf/Infof/Warnf/Errorf 是给新代码用的便捷入口。
func Debugf(format string, args ...interface{}) { Log(DebugLevel, fmt.Sprintf(format, args...), nil) }
func Infof(format string, args ...interface{})  { Log(InfoLevel, fmt.Sprintf(format, args...), nil) }
func Warnf(format string, args ...interface{})  { Log(WarnLevel, fmt.Sprintf(format, args...), nil) }
func Errorf(format string, args ...interface{}) { Log(ErrorLevel, fmt.Sprintf(format, args...), nil) }

// Dropped 返回因队列满被丢弃的行数（进程内累计）。
func Dropped() uint64 {
	if l := std.Load(); l != nil {
		return l.dropped.Load()
	}
	return 0
}

// Dir 返回当前日志目录（未初始化时为空）。
func Dir() string {
	if l := std.Load(); l != nil {
		return l.cfg.Dir
	}
	return ""
}

// ---- 存量 log.Printf 适配 ----

// Writer 返回给 log.SetOutput 使用的适配器。
//
// 存量代码的日志都带 [xxx] 前缀（[error]/[retry]/[usage]…），
// 按前缀推断级别即可，不必逐个改调用点。
func Writer() io.Writer { return writerAdapter{} }

type writerAdapter struct{}

func (writerAdapter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	Log(inferLevel(msg), msg, nil)
	return len(p), nil
}

func inferLevel(msg string) Level {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(msg, "[error]"), strings.Contains(msg, "Fatal"), strings.Contains(lower, "failed"):
		return ErrorLevel
	case strings.Contains(msg, "[retry]"), strings.Contains(msg, "[warn]"), strings.Contains(msg, "⚠️"):
		return WarnLevel
	case strings.Contains(msg, "[debug]"):
		return DebugLevel
	default:
		return InfoLevel
	}
}

// ---- 脱敏 ----

// sensitiveKeys 是绝不能出现在日志里的凭据字段。
var sensitiveKeys = []string{
	"xiaomichatbot_ph=",
	"xiaomichatbot_serviceToken=",
	"serviceToken=",
	"service_token=",
	"userId=",
	"user_id=",
	"ph=",
	"Authorization: Bearer ",
	"authorization: bearer ",
}

// Redact 抹掉字符串里可能混进来的凭据值。
// 上游报错正文偶尔回显请求 URL，里面就带着 xiaomichatbot_ph。
//
// 注意搜索起点必须往前推：替换后 key 本身仍然留在原处，
// 每次从 0 开始找会反复命中同一个 key（并不断追加 ***），直接死循环。
func Redact(s string) string {
	const mask = "***"
	for _, key := range sensitiveKeys {
		from := 0
		for {
			i := strings.Index(s[from:], key)
			if i < 0 {
				break
			}
			i += from
			j := i + len(key)
			k := j
			for k < len(s) && !isValueEnd(s[k]) {
				k++
			}
			s = s[:j] + mask + s[k:]
			from = j + len(mask)
		}
	}
	return s
}

func isValueEnd(c byte) bool {
	switch c {
	case '&', ' ', '"', '\'', '\n', '\r', '\t', ',', ';':
		return true
	}
	return false
}

// ---- 读写 ----

func (l *Logger) run() {
	defer close(l.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	pending := 0
	write := func(b []byte) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.bw == nil {
			return
		}
		if _, err := l.bw.Write(b); err != nil {
			fmt.Fprintf(os.Stderr, "logging: write failed: %v\n", err)
			return
		}
		pending += len(b)
	}

	for {
		select {
		case b := <-l.ch:
			l.mu.Lock()
			// 跨天自动切文件
			_ = l.rotateLocked(time.Now())
			l.mu.Unlock()
			write(b)
			if pending >= flushThreshold {
				l.mu.Lock()
				l.flushLocked()
				l.mu.Unlock()
				pending = 0
			}
		case <-ticker.C:
			l.mu.Lock()
			l.flushLocked()
			l.mu.Unlock()
			pending = 0
		case <-l.stop:
			// 排空队列后退出：停在半路的日志比丢日志更糟
			for {
				select {
				case b := <-l.ch:
					write(b)
				default:
					l.mu.Lock()
					l.closeLocked()
					l.mu.Unlock()
					return
				}
			}
		}
	}
}

func (l *Logger) flushLocked() {
	if l.bw != nil {
		_ = l.bw.Flush()
	}
}

// Tail 读取当天日志的最后 n 行（给管理端排查用）。
func Tail(n int) ([]string, error) {
	l := std.Load()
	if l == nil {
		return nil, fmt.Errorf("logging not initialized")
	}
	if n <= 0 || n > 2000 {
		n = 200
	}
	// 缓冲里的行还没落盘，先刷一次再读，否则刚出的日志看不到
	l.mu.Lock()
	l.flushLocked()
	l.mu.Unlock()

	path := filepath.Join(l.cfg.Dir, time.Now().Format(dateLayout)+".log")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
