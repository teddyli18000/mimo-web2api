package pool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teddyli18000/mimo-web2api/internal/config"
	"github.com/teddyli18000/mimo-web2api/internal/mimo"
)

func testAccounts(n int) []config.Account {
	out := make([]config.Account, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, config.Account{
			ID:           string(rune('a' + i)),
			ServiceToken: "token-" + string(rune('a'+i)),
			UserID:       "user-" + string(rune('a'+i)),
			Ph:           "ph-" + string(rune('a'+i)),
			Active:       true,
		})
	}
	return out
}

func countDown(p *Pool) (down, up int) {
	for _, s := range p.Status() {
		if s.Healthy {
			up++
		} else {
			down++
		}
	}
	return
}

// 认证失效（401/403/451）必须立刻摘除：用户重新同步 Cookie 之前，
// 任何一次落到这个账号上的请求都会白等一轮。
func TestReportAuthErrorRemovesImmediately(t *testing.T) {
	p := New(testAccounts(2))
	client, err := p.Next()
	if err != nil {
		t.Fatal(err)
	}

	p.Report(client, &mimo.HTTPError{Status: 401, Body: "login required"})

	down, up := countDown(p)
	if down != 1 || up != 1 {
		t.Fatalf("期望 1 个摘除 1 个健康，实际 down=%d up=%d", down, up)
	}
	for _, s := range p.Status() {
		if s.Healthy {
			continue
		}
		if s.Failures != 1 {
			t.Fatalf("失败次数应为 1，实际 %d", s.Failures)
		}
		if !strings.Contains(s.LastError, "401") {
			t.Fatalf("最后错误应保留状态码: %q", s.LastError)
		}
		if s.DownSince == nil {
			t.Fatal("摘除时应记录 down_since")
		}
	}
}

// 普通 5xx 不立刻摘除：一次抖动就把账号踢出去，多账号池会退化成单账号。
func TestGenericFailuresNeedThreshold(t *testing.T) {
	p := New(testAccounts(1))
	client, _ := p.Next()
	boom := errors.New("mimo returned 500: boom")

	p.Report(client, boom)
	p.Report(client, boom)
	if down, _ := countDown(p); down != 0 {
		t.Fatal("连续两次失败不应摘除")
	}

	p.Report(client, boom)
	if down, _ := countDown(p); down != 1 {
		t.Fatal("连续三次失败应摘除")
	}
	if got := p.Status()[0].Failures; got != 3 {
		t.Fatalf("失败次数应为 3，实际 %d", got)
	}
}

func TestNextSkipsDownAccount(t *testing.T) {
	p := New(testAccounts(2))
	bad, _ := p.Next()
	p.Report(bad, &mimo.HTTPError{Status: 403})

	for i := 0; i < 10; i++ {
		got, err := p.Next()
		if err != nil {
			t.Fatal(err)
		}
		if got == bad {
			t.Fatal("已摘除的账号仍在轮询")
		}
	}
}

// 冷却到期后放一次半开试探；试探成功即恢复。
func TestCooldownHalfOpenRecovers(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.Local)
	p := New(testAccounts(1))
	p.now = func() time.Time { return now }

	client, _ := p.Next()
	p.Report(client, &mimo.HTTPError{Status: 401})
	if p.Status()[0].Healthy {
		t.Fatal("认证失效后应为不健康")
	}

	// 冷却期内不放行
	if got, _ := p.Next(); got != client {
		t.Fatal("只有一个账号时仍然会兜底同一个客户端")
	}
	if p.Status()[0].Probing {
		t.Fatal("冷却期内不应进入试探")
	}

	now = now.Add(cooldown + time.Second)
	if _, err := p.Next(); err != nil {
		t.Fatal(err)
	}
	if !p.Status()[0].Probing {
		t.Fatal("冷却到期后应发起半开试探")
	}

	p.Report(client, nil)
	st := p.Status()[0]
	if !st.Healthy || st.Probing || st.Failures != 0 || st.LastError != "" {
		t.Fatalf("试探成功后应完全恢复: %+v", st)
	}
}

// 客户端主动断连不算账号失败。
func TestClientCancelNotCounted(t *testing.T) {
	p := New(testAccounts(1))
	client, _ := p.Next()

	p.Report(client, context.Canceled)
	p.Report(client, context.DeadlineExceeded)
	p.Report(client, errors.New("stream closed by client: context canceled"))

	st := p.Status()[0]
	if !st.Healthy || st.Failures != 0 {
		t.Fatalf("断连不应计入失败: %+v", st)
	}
}

// Reload 只重建客户端，不能把坏账号「治好」；
// 但凭据变了说明用户重新同步过 Cookie，必须重置。
func TestReloadInheritsHealthUnlessCredentialsChange(t *testing.T) {
	accounts := testAccounts(2)
	p := New(accounts)
	bad, _ := p.Next()
	p.Report(bad, &mimo.HTTPError{Status: 401})

	var badID string
	for _, s := range p.Status() {
		if !s.Healthy {
			badID = s.ID
		}
	}
	if badID == "" {
		t.Fatal("没找到被摘除的账号")
	}

	p.Reload(accounts)
	for _, s := range p.Status() {
		if s.ID == badID && s.Healthy {
			t.Fatal("Reload 不应重置健康状态")
		}
	}

	// 同一账号换新 token
	updated := make([]config.Account, len(accounts))
	copy(updated, accounts)
	for i := range updated {
		if updated[i].ID == badID {
			updated[i].ServiceToken = "brand-new-token"
		}
	}
	p.Reload(updated)
	for _, s := range p.Status() {
		if s.ID == badID && !s.Healthy {
			t.Fatal("凭据更新后账号应重新参与轮询")
		}
	}
}

func TestInactiveAccountsSkipped(t *testing.T) {
	accounts := testAccounts(2)
	accounts[1].Active = false
	p := New(accounts)
	if p.Count() != 1 {
		t.Fatalf("禁用账号不应进池: %d", p.Count())
	}
}

func TestNextWithoutAccounts(t *testing.T) {
	p := New(nil)
	if _, err := p.Next(); err == nil {
		t.Fatal("空池应返回错误")
	}
	if p.HasAccounts() {
		t.Fatal("空池 HasAccounts 应为 false")
	}
}
