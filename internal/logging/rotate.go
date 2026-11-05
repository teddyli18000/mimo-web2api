package logging

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// dateLayout 日志文件名用的日期格式：data/logs/2026-11-05.log
	dateLayout = "2006-01-02"
	// cleanupInterval 清理周期。保留期是「天」级别，6 小时足够；
	// Init 时也会立刻清一次，所以进程重启也能收敛。
	cleanupInterval = 6 * time.Hour
)

// rotateLocked 确保当前文件是 now 所属日期的文件；已持锁。
//
// 按天切分而不是按大小：排查时「昨天/今天」是最自然的切分维度，
// 单个文件也不会大到打不开（正常网关一天几十 MB 量级）。
func (l *Logger) rotateLocked(now time.Time) error {
	date := now.Format(dateLayout)
	if l.file != nil && l.curDate == date {
		return nil
	}
	if l.bw != nil {
		_ = l.bw.Flush()
	}
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
		l.bw = nil
	}

	path := filepath.Join(l.cfg.Dir, date+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	l.file = f
	l.bw = bufio.NewWriterSize(f, flushThreshold)
	l.curDate = date

	// 切新文件时顺手清理一次：即使 ticker 没跑到，也不会让过期文件堆积
	l.cleanupLocked(now)
	return nil
}

// cleanupLoop 周期性清理过期日志。
func (l *Logger) cleanupLoop() {
	t := time.NewTicker(cleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case now := <-t.C:
			l.mu.Lock()
			l.cleanupLocked(now)
			l.mu.Unlock()
		}
	}
}

// cleanupLocked 删除超过保留期的日志文件；已持锁。
//
// 判定顺序：文件名里的日期 → 解析失败时回落 mtime。
// 只处理 *.log，不碰目录里的其它文件。
func (l *Logger) cleanupLocked(now time.Time) {
	entries, err := os.ReadDir(l.cfg.Dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-l.cfg.Retention)
	removed := 0

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		// 正在写的文件永不删除
		if e.Name() == l.curDate+".log" {
			continue
		}

		day := strings.TrimSuffix(e.Name(), ".log")
		ts, perr := time.ParseInLocation(dateLayout, day, time.Local)
		if perr != nil {
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			ts = info.ModTime()
		}
		if ts.After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(l.cfg.Dir, e.Name())); err != nil {
			continue
		}
		removed++
	}

	if removed > 0 {
		fmt.Fprintf(os.Stderr, "logging: 清理了 %d 个超过 %d 天的日志文件\n",
			removed, int(l.cfg.Retention.Hours()/24))
	}
}
