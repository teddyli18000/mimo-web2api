package promptclean

import (
	"strings"
	"testing"
)

func withMode(t *testing.T, m Mode) {
	t.Helper()
	old := CurrentMode()
	SetMode(m)
	t.Cleanup(func() { SetMode(old) })
}

// 真实 DSH 系统提示里的三处指纹（顺序、大小写、措辞照抄 fixture）
const dshFixture = `You are an AI agent powered by DeepSeek Harness.

You are a coding agent powered by the mimo-v2.6-pro model.

Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root.

The runtime context names the DeepSeek Harness implementation checkout.

You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names. When the user refers to "this page", they mean the GUI.`

func TestConservativeRewritesOnlyFingerprints(t *testing.T) {
	withMode(t, ModeConservative)
	out, hits := Clean(dshFixture, true)

	if len(hits) != 3 {
		t.Fatalf("应命中 gui-order / file-reference / identity-clause 三条（identity 一次替换两处），实际 %v", hits)
	}
	// 零信息损失：产品名、模型名、@ 引用规则、GUI 指代全部保留
	for _, want := range []string{
		"DeepSeek Harness",
		"mimo-v2.6-pro",
		"Tokens prefixed with @",
		"Relative paths resolve from the workspace root",
		`"this page"`,
		"The runtime context names the DeepSeek Harness implementation checkout.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("清洗后丢了关键信息 %q\n%s", want, out)
		}
	}
	// 连续指纹被打断
	for _, gone := range []string{
		"are paths the user explicitly referenced",
		"whose URL the runtime context names",
		"You are an AI agent powered by",
		"You are a coding agent powered by",
	} {
		if strings.Contains(out, gone) {
			t.Errorf("指纹仍在: %q", gone)
		}
	}
}

func TestCleanIsIdempotent(t *testing.T) {
	withMode(t, ModeAggressive)
	once, _ := Clean(dshFixture, true)
	twice, hits := Clean(once, true)
	if once != twice {
		t.Fatalf("不幂等:\n第一次: %s\n\n第二次: %s", once, twice)
	}
	if len(hits) != 0 {
		t.Fatalf("第二次仍命中规则: %v", hits)
	}
}

// 参考实现 #537：user / tool 里的同句属用户数据，一律逐字不动
func TestUserDataNeverRewritten(t *testing.T) {
	withMode(t, ModeAggressive)
	text := "Tokens prefixed with @ are paths the user explicitly referenced. " +
		"You are an AI agent powered by DeepSeek Harness."
	for _, role := range []string{"user", "tool", "function"} {
		out, hits := Clean(text, identityRole(role))
		if out != text || len(hits) != 0 {
			t.Errorf("role=%s 的用户数据被改写了", role)
		}
	}
	for _, role := range []string{"system", "developer", "assistant"} {
		if !identityRole(role) {
			t.Errorf("role=%s 应允许清洗", role)
		}
	}
}

func TestOffModeIsNoop(t *testing.T) {
	withMode(t, ModeOff)
	out, hits := Clean(dshFixture, true)
	if out != dshFixture || len(hits) != 0 {
		t.Fatal("off 模式不该改写任何内容")
	}
	rts := [][2]string{{"system", dshFixture}, {"user", "keep me"}}
	cleaned, stats := CleanRoleTexts(rts)
	if len(stats) != 0 || cleaned[0][1] != dshFixture {
		t.Fatal("off 模式下 CleanRoleTexts 应原样返回")
	}
}

func TestAggressiveOnlyAddsBrandShortening(t *testing.T) {
	withMode(t, ModeConservative)
	c, _ := Clean("The DeepSeek Harness checkout is here.", true)
	if c != "The DeepSeek Harness checkout is here." {
		t.Fatalf("conservative 不该动品牌词: %q", c)
	}

	withMode(t, ModeAggressive)
	a, hits := Clean("The DeepSeek Harness checkout is here.", true)
	if a != "The DeepSeek checkout is here." {
		t.Fatalf("aggressive 品牌缩减不对: %q", a)
	}
	if len(hits) != 1 || hits[0] != "brand-shorten" {
		t.Fatalf("命中规则不对: %v", hits)
	}
	// 路径/标识符不能被改坏（参考实现 #574）
	for _, path := range []string{"/opt/deepseek-harness/app", "deepseek_harness", "deepseek-harness"} {
		out, _ := Clean(path, true)
		if out != path {
			t.Errorf("路径被改坏: %q -> %q", path, out)
		}
	}
}

func TestCleanRoleTextsKeepsOriginalForFingerprint(t *testing.T) {
	withMode(t, ModeConservative)
	original := [][2]string{
		{"system", dshFixture},
		{"user", "Tokens prefixed with @ are paths the user explicitly referenced."},
		{"tool", "file result"},
	}
	cleaned, stats := CleanRoleTexts(original)

	if len(stats) == 0 {
		t.Fatal("应有命中统计")
	}
	// 入参必须原封不动：convstore fingerprint 用的是这份
	if original[0][1] != dshFixture {
		t.Fatal("CleanRoleTexts 改了入参，会话指纹会跟着变")
	}
	// system 改写、user/tool 逐字保留
	if cleaned[0][1] == dshFixture {
		t.Fatal("system 没被清洗")
	}
	if cleaned[1][1] != original[1][1] || cleaned[2][1] != original[2][1] {
		t.Fatal("user/tool 被改写了")
	}
}

func TestNoFalsePositiveOnPlainText(t *testing.T) {
	withMode(t, ModeAggressive)
	for _, text := range []string{
		"请读一下 README.md，再用 grep 找 TODO。",
		"The build is powered by Go 1.23 and tested in CI.",
		"Use the read tool — not shell commands like cat.",
	} {
		out, hits := Clean(text, true)
		if out != text || len(hits) != 0 {
			t.Errorf("普通文本被误改写 %q -> %q (%v)", text, out, hits)
		}
	}
}
