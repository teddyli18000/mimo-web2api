package pool

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/teddyli18000/mimo-web2api/internal/config"
	"github.com/teddyli18000/mimo-web2api/internal/logging"
	"github.com/teddyli18000/mimo-web2api/internal/mimo"
)

const (
	// maxFailures 非认证类错误连续失败到这个次数就摘除。
	// 不设成 1：一次网络抖动、一次上游 5xx 就把账号踢出轮询，
	// 多账号池会退化成单账号甚至全灭。
	maxFailures = 3
	// cooldown 摘除后的冷却时间。到期后放一个请求做半开试探，
	// 成功即恢复；失败则重新计时。
	cooldown = 2 * time.Minute
	// fallbackWarnInterval 全不可用时的告警节流：轮询期没有健康账号，
	// 每个请求都打一条日志会瞬间把日志刷满。
	fallbackWarnInterval = 30 * time.Second
)

var errNoAccounts = errors.New("no accounts configured")

type Pool struct {
	clients []*entry
	counter atomic.Uint64
	mu      sync.RWMutex

	// now 可注入的时钟（测试用）。生产环境为 nil，走 time.Now。
	now func() time.Time
	// lastFallbackWarn 上一次「全部账号不可用」告警时间，用于节流。
	lastFallbackWarn time.Time
}

type entry struct {
	account config.Account
	client  *mimo.WebClient
	healthy bool

	failures int       // 连续失败次数（成功即清零）
	lastErr  string    // 最后一次错误（已脱敏、已截断）
	downAt   time.Time // 摘除时刻，冷却起点
	probing  bool      // 半开试探进行中，避免所有流量同时压到一个账号
}

// AccountStatus 是账号健康状态的只读快照，给管理面板用。
type AccountStatus struct {
	ID        string     `json:"id"`
	Healthy   bool       `json:"healthy"`
	Probing   bool       `json:"probing"`
	Failures  int        `json:"failures"`
	LastError string     `json:"last_error,omitempty"`
	DownSince *time.Time `json:"down_since,omitempty"`
}

func New(accounts []config.Account) *Pool {
	p := &Pool{}
	for _, acc := range accounts {
		if !acc.Active {
			continue
		}
		p.clients = append(p.clients, newEntry(acc))
	}
	return p
}

func newEntry(acc config.Account) *entry {
	return &entry{
		account: acc,
		client:  mimo.NewWebClient(acc.ServiceToken, acc.UserID, acc.Ph),
		healthy: true,
	}
}

func (p *Pool) nowTime() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// Next 获取下一个可用客户端。
//
// 选择顺序：健康账号轮询 → 冷却已过期的账号做一次半开试探（同一时刻只放一个）
// → 全部不可用时兜底第一个（宁可试错也别让请求直接 503）。
func (p *Pool) Next() (*mimo.WebClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.clients)
	if n == 0 {
		return nil, errNoAccounts
	}
	for i := 0; i < n; i++ {
		idx := int(p.counter.Add(1)) % n
		e := p.clients[idx]
		if e.healthy {
			return e.client, nil
		}
	}

	now := p.nowTime()
	for i := 0; i < n; i++ {
		idx := int(p.counter.Add(1)) % n
		e := p.clients[idx]
		if e.probing || e.downAt.IsZero() || now.Sub(e.downAt) < cooldown {
			continue
		}
		e.probing = true
		log.Printf("[pool] 账号 %s 冷却结束，发起半开试探", e.account.ID)
		return e.client, nil
	}

	if now.Sub(p.lastFallbackWarn) >= fallbackWarnInterval {
		p.lastFallbackWarn = now
		log.Printf("[pool] 所有账号均不可用，临时回退到 %s", p.clients[0].account.ID)
	}
	return p.clients[0].client, nil
}

// Report 上报一次请求结果，驱动账号健康状态机。
//
// err == nil：成功，清零失败计数并按需恢复账号。
// err != nil：认证错误立即摘除；其它错误累计到 maxFailures 再摘除。
// 客户端主动断连不算账号问题（否则用户关掉对话框就能把账号打成不可用）。
func (p *Pool) Report(client *mimo.WebClient, err error) {
	if client == nil {
		return
	}
	if err != nil && isClientSide(err) {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	e := p.findLocked(client)
	if e == nil {
		return
	}

	if err == nil {
		if !e.healthy {
			log.Printf("[pool] 账号 %s 恢复轮询", e.account.ID)
		}
		e.healthy = true
		e.failures = 0
		e.lastErr = ""
		e.downAt = time.Time{}
		e.probing = false
		return
	}

	e.failures++
	e.lastErr = truncate(logging.Redact(err.Error()), 200)

	auth := mimo.IsAuthError(err)
	if !auth && e.failures < maxFailures {
		return
	}
	// 已经摘除的账号重复失败不重复打日志（半开试探除外）
	wasDown := !e.healthy
	e.healthy = false
	e.downAt = p.nowTime()
	e.probing = false
	if !wasDown {
		reason := fmt.Sprintf("%d 次失败", e.failures)
		if auth {
			reason = "认证失效"
		}
		log.Printf("[pool] 账号 %s 已摘除（%s）：%s", e.account.ID, reason, e.lastErr)
	}
}

// AccountID 返回客户端对应的账号 ID；找不到返回空串。
// 日志只需要 ID，不需要凭据。
func (p *Pool) AccountID(client *mimo.WebClient) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e := p.findLocked(client); e != nil {
		return e.account.ID
	}
	return ""
}

func (p *Pool) findLocked(client *mimo.WebClient) *entry {
	for _, e := range p.clients {
		if e.client == client {
			return e
		}
	}
	return nil
}

// Status 返回所有账号的健康快照。
func (p *Pool) Status() []AccountStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]AccountStatus, 0, len(p.clients))
	for _, e := range p.clients {
		st := AccountStatus{
			ID:        e.account.ID,
			Healthy:   e.healthy,
			Probing:   e.probing,
			Failures:  e.failures,
			LastError: e.lastErr,
		}
		if !e.downAt.IsZero() {
			t := e.downAt
			st.DownSince = &t
		}
		out = append(out, st)
	}
	return out
}

func (p *Pool) HasAccounts() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.clients) > 0
}

// Reload 按新配置重建账号池。
//
// 健康状态按账号 ID 继承：改一个账号的备注/启用状态，不该把其它
// 已被摘除的坏账号一并「治好」——那会让下一个请求又打到失效 Cookie 上。
// 但凭据变了（重新同步了 Cookie）就当作新账号，重置为健康。
func (p *Pool) Reload(accounts []config.Account) {
	p.mu.Lock()
	defer p.mu.Unlock()

	old := make(map[string]*entry, len(p.clients))
	for _, e := range p.clients {
		old[e.account.ID] = e
	}

	next := make([]*entry, 0, len(accounts))
	for _, acc := range accounts {
		if !acc.Active {
			continue
		}
		e := newEntry(acc)
		if prev, ok := old[acc.ID]; ok && sameCredential(prev.account, acc) {
			e.healthy = prev.healthy
			e.failures = prev.failures
			e.lastErr = prev.lastErr
			e.downAt = prev.downAt
			e.probing = prev.probing
		}
		next = append(next, e)
	}
	p.clients = next
}

func sameCredential(a, b config.Account) bool {
	return a.ServiceToken == b.ServiceToken && a.UserID == b.UserID && a.Ph == b.Ph
}

func (p *Pool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.clients)
}

// HealthCheck 逐个校验账号可用性。
// 校验是网络请求（可达数十秒），必须在锁外做：持写锁会让所有聊天请求的
// Next() 一起卡住（2026-09 审查发现）。
func (p *Pool) HealthCheck(ctx context.Context) map[string]bool {
	p.mu.RLock()
	snapshot := make([]*entry, 0, len(p.clients))
	for _, e := range p.clients {
		snapshot = append(snapshot, e)
	}
	p.mu.RUnlock()

	results := make(map[string]bool, len(snapshot))
	for _, e := range snapshot {
		healthy := e.client.Validate(ctx) == nil
		results[e.account.ID] = healthy
	}

	now := p.nowTime()
	p.mu.Lock()
	for _, e := range snapshot {
		h, ok := results[e.account.ID]
		if !ok {
			continue
		}
		e.healthy = h
		e.probing = false
		if h {
			e.failures = 0
			e.lastErr = ""
			e.downAt = time.Time{}
			continue
		}
		e.failures++
		e.lastErr = "健康检查失败"
		e.downAt = now
	}
	p.mu.Unlock()
	return results
}

// isClientSide 客户端侧中断（用户关页面、上游已取消）。
func isClientSide(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, context.Canceled.Error())
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// 不切坏 UTF-8：回退到 rune 边界
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
