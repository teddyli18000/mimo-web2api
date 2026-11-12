package promptclean

import (
	"regexp"
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

// 严格版「数据无损」：把三条保守改写逐个逆回去，必须逐字还原成原文。
// 只检查关键词是否还在，无法发现「多余的空白被吃掉」「另一行被改动」这类损失。
func TestConservativeIsLossless(t *testing.T) {
	withMode(t, ModeConservative)
	out, _ := Clean(dshFixture, true)

	restored := out
	for _, inv := range []struct {
		re  *regexp.Regexp
		rep string
	}{
		// 逆序应用：与 Clean 里的规则顺序相反
		{regexp.MustCompile(`(?i)(You are (?:an?|the) (?:[\w.-]+\s+){0,3}?(?:agent|assistant|model|AI|LLM)) that is (powered by|built on|built by|created by|developed by|made by|trained by)`), "${1} ${2}"},
		{regexp.MustCompile(`(?i)(Tokens prefixed with @ are paths )that (the user explicitly referenced\.)`), "${1}${2}"},
		{regexp.MustCompile(`(?i)(You are interacting with the user through the [\w ._-]{1,60}? Web GUI) whose URL is named in the runtime context\.`), "${1} whose URL the runtime context names."},
	} {
		restored = inv.re.ReplaceAllString(restored, inv.rep)
	}
	if restored != dshFixture {
		t.Fatalf("清洗不是零信息损失：逆还原后与原文不一致\n原文:\n%s\n\n逆还原:\n%s", dshFixture, restored)
	}
}

// 快路径等价：跳过 needle 检查、对每条规则都跑正则，结果必须与 Clean 完全一致。
// 否则某个 needle 写窄了会静默漏掉本该改写的句子。
func TestFastPathEquivalence(t *testing.T) {
	for _, m := range []Mode{ModeConservative, ModeAggressive} {
		withMode(t, m)
		for _, text := range []string{
			dshFixture,
			"The build is powered by Go and made by the team.", // needle 命中但正则不匹配
			"请读一下 README.md，再用 grep 找 TODO。",                 // 无 needle
			"DeepSeek Harness is at /opt/deepseek-harness/app.", // brand-shorten 只该改散文
			"You are an AI agent trained by the team.",           // 正则有 trained by，needle 曾漏掉
			"You are a coding agent built on an open model.",     // built on 分支
		} {
			got, gotHits := Clean(text, true)

			want := text
			var wantHits []string
			for _, r := range rules {
				if r.aggressive && m != ModeAggressive {
					continue
				}
				next := r.re.ReplaceAllString(want, r.replace)
				if next != want {
					want = next
					wantHits = append(wantHits, r.name)
				}
			}
			if got != want || strings.Join(gotHits, ",") != strings.Join(wantHits, ",") {
				t.Errorf("mode=%s 快路径与全量正则不等价\ntext=%q\nClean   =%q (%v)\nno-fast =%q (%v)",
					m, text, got, gotHits, want, wantHits)
			}
		}
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
