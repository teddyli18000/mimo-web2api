package logging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInitWritesJSONLAndFiltersByLevel(t *testing.T) {
	dir := t.TempDir()
	if err := Init(Config{Dir: dir, Console: false, Level: InfoLevel, Retention: 7 * 24 * time.Hour}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	Log(InfoLevel, "hello", map[string]interface{}{"req_id": "abc123", "model": "mimo-v2.6-pro"})
	Log(DebugLevel, "should be filtered", nil)
	Shutdown()

	path := filepath.Join(dir, time.Now().Format(dateLayout)+".log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读日志失败: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("debug 级别应被过滤，实际有 %d 行:\n%s", len(lines), data)
	}

	var rec map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("不是合法 JSONL: %v (%s)", err, lines[0])
	}
	if rec["msg"] != "hello" || rec["level"] != "info" {
		t.Fatalf("字段不对: %v", rec)
	}
	if rec["req_id"] != "abc123" || rec["model"] != "mimo-v2.6-pro" {
		t.Fatalf("自定义字段丢失: %v", rec)
	}
	if _, ok := rec["ts"].(string); !ok {
		t.Fatalf("缺少时间戳: %v", rec)
	}
}

// 超过保留期的文件必须删掉，保留期内的必须留下，正在写的文件永不删。
func TestCleanupRemovesExpiredFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.Local)

	write := func(offset int) string {
		name := now.AddDate(0, 0, offset).Format(dateLayout) + ".log"
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return name
	}

	expired8 := write(-8)
	expired7 := write(-7)
	recent := write(-1)
	today := write(0)

	l := &Logger{cfg: Config{Dir: dir, Retention: 7 * 24 * time.Hour}, curDate: now.Format(dateLayout)}
	l.cleanupLocked(now)

	for _, name := range []string{expired8, expired7} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s 应该被清理", name)
		}
	}
	for _, name := range []string{recent, today} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s 不该被清理: %v", name, err)
		}
	}
}

// 跨天自动换文件，且旧文件内容不会被写到新文件里。
func TestRotateSplitsByDay(t *testing.T) {
	dir := t.TempDir()
	l := &Logger{cfg: Config{Dir: dir, Retention: 7 * 24 * time.Hour}}
	defer func() {
		if l.file != nil {
			_ = l.file.Close()
		}
	}()

	day1 := time.Date(2026, 11, 4, 23, 0, 0, 0, time.Local)
	if err := l.rotateLocked(day1); err != nil {
		t.Fatal(err)
	}
	if _, err := l.bw.WriteString("day1\n"); err != nil {
		t.Fatal(err)
	}

	day2 := day1.Add(2 * time.Hour)
	if err := l.rotateLocked(day2); err != nil {
		t.Fatal(err)
	}
	if _, err := l.bw.WriteString("day2\n"); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	l.flushLocked()
	l.mu.Unlock()

	read := func(day time.Time) string {
		data, err := os.ReadFile(filepath.Join(dir, day.Format(dateLayout)+".log"))
		if err != nil {
			t.Fatalf("读 %s 失败: %v", day.Format(dateLayout), err)
		}
		return string(data)
	}
	if got := read(day1); got != "day1\n" {
		t.Fatalf("第一天内容串了: %q", got)
	}
	if got := read(day2); got != "day2\n" {
		t.Fatalf("第二天内容串了: %q", got)
	}
}

// 队列满时必须丢弃而不是阻塞请求路径。
func TestLogNeverBlocksWhenQueueFull(t *testing.T) {
	l := &Logger{cfg: Config{Dir: t.TempDir()}, ch: make(chan []byte, 1)}
	prev := std.Swap(l)
	defer std.Store(prev)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			Log(InfoLevel, "flood", nil)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("队列满时 Log 阻塞了")
	}
	if l.dropped.Load() == 0 {
		t.Fatal("应记录被丢弃的行数")
	}
}

func TestRedact(t *testing.T) {
	in := "GET /open?xiaomichatbot_ph=%22secret%22&userId=42 serviceToken=abc"
	out := Redact(in)
	for _, leak := range []string{"%22secret%22", "userId=42", "serviceToken=abc"} {
		if strings.Contains(out, leak) {
			t.Fatalf("凭据没被抹掉 (%s): %s", leak, out)
		}
	}
	if !strings.Contains(out, "xiaomichatbot_ph=***") {
		t.Fatalf("结构被破坏了: %s", out)
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"debug":   DebugLevel,
		"INFO":    InfoLevel,
		"warn":    WarnLevel,
		"error":   ErrorLevel,
		"unknown": InfoLevel,
		"":        InfoLevel,
	}
	for in, want := range cases {
		if got := ParseLevel(in); got != want {
			t.Fatalf("ParseLevel(%q)=%v want %v", in, got, want)
		}
	}
}
