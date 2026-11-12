// Package promptclean 在把客户端提示词交给上游之前，打断已被确认会触发
// 上游内容审核的「连续短语」指纹。
//
// 设计借鉴 ardeyouxipianyi/workbuddy2api-hub 的 sanitize_messages：
//   - 规则表驱动，只改写已确认的连续短语，一个词都不删、语义不变
//   - 作用域限定 system / developer / assistant；user / tool 是用户数据，逐字不动
//   - 幂等（替换结果不再匹配原规则）
//   - 小写字面量快路径：没命中就不跑正则
//
// 但规则集不是照抄：参考实现的规则是针对 WorkBuddy / DeepSeek 渠道校验
// （400 / code 11-128）逐段二分实测出来的，我们的上游是 MiMo 网页端，审核
// 规则不同。这里只保留「零信息损失」的改写——补一个词、换一处语序，句子
// 意思和给模型的信息量完全不变；删词/整句中性化的规则放进 aggressive，
// 默认不启用。
package promptclean

import (
	"os"
	"regexp"
	"strings"
	"sync/atomic"
)

// Mode 清洗强度。
type Mode int32

const (
	// ModeOff 完全不改写。
	ModeOff Mode = iota
	// ModeConservative 默认：只做零信息损失的语序/补词改写。
	ModeConservative
	// ModeAggressive 额外启用品牌词缩减（会丢一个词，仅在确有必要时开）。
	ModeAggressive
)

var mode atomic.Int32

func init() {
	mode.Store(int32(parseMode(os.Getenv("PROMPT_CLEAN"))))
}

func parseMode(s string) Mode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "off", "false", "no", "disabled":
		return ModeOff
	case "aggressive", "strict", "full":
		return ModeAggressive
	default:
		return ModeConservative
	}
}

// String 返回配置里使用的名字，日志与面板用它。
func (m Mode) String() string {
	switch m {
	case ModeOff:
		return "off"
	case ModeAggressive:
		return "aggressive"
	default:
		return "conservative"
	}
}

// CurrentMode 返回当前生效的模式。
func CurrentMode() Mode { return Mode(mode.Load()) }

// SetMode 覆盖模式（测试与运行时切换用）。
func SetMode(m Mode) { mode.Store(int32(m)) }

type rule struct {
	name       string
	needles    []string // 小写字面量；任一命中才进入正则
	re         *regexp.Regexp
	replace    string
	aggressive bool
}

// 每条规则的替换结果都不再包含自己的 needle，因此重复调用不会叠加。
var rules = []rule{
	{
		// DSH GUI 描述段（参考实现 #570）：上游按「A 句 + B 句连续出现」判定，
		// 只把句尾语序换一下，一个词都不删，品牌全名也保留。
		name:    "gui-order",
		needles: []string{"whose url the runtime context names"},
		re: regexp.MustCompile(`(?i)(You are interacting with the user through the [\w ._-]{1,60}? Web GUI)` +
			` whose URL the runtime context names\.`),
		replace: "${1} whose URL is named in the runtime context.",
	},
	{
		// 文件引用段（参考实现 #683）：整段 5 句连排命中；只给 A 句补一个
		// 关系代词 "that"，语法等价、信息量不变，连续匹配被打断。
		name:    "file-reference",
		needles: []string{"are paths the user explicitly referenced"},
		re:      regexp.MustCompile(`(?i)(Tokens prefixed with @ are paths )(the user explicitly referenced\.)`),
		replace: "${1}that ${2}",
	},
	{
		// 客户端自述身份句：把 "You are <agent> powered by X." 改成
		// "You are <agent> that is powered by X."，不删产品名也不删模型名，
		// 打断 "You are … powered by …" 这一连续模式。
		// 正则要求 powered 前紧邻 agent/assistant/model 等角色词，替换后
		// 紧邻的是 "is"，因此幂等。
		name:    "identity-clause",
		// needle 必须覆盖正则里的每一个动词短语，否则那个分支会被快路径静默跳过
		needles: []string{" powered by ", " built on ", " built by ", " created by ", " developed by ", " made by ", " trained by "},
		re: regexp.MustCompile(`(?i)(You are (?:an?|the) (?:[\w.-]+\s+){0,3}?` +
			`(?:agent|assistant|model|AI|LLM)) ` +
			`(powered by|built on|built by|created by|developed by|made by|trained by)`),
		replace: "${1} that is ${2}",
	},
	{
		// 品牌词缩减（参考实现 #574）：唯一会丢词的规则。默认不开。
		// 只匹配散文中的空格/制表符形式，不碰 deepseek-harness 这类路径与标识符。
		name:       "brand-shorten",
		needles:    []string{"harness"},
		re:         regexp.MustCompile(`(?i)DeepSeek[ \t]+Harness`),
		replace:    "DeepSeek",
		aggressive: true,
	},
}

// identityRoles 允许改写身份指纹的角色。user / tool / function 是用户数据，
// 一律逐字不动（参考实现 #537 的职责边界）。
func identityRole(role string) bool {
	switch strings.ToLower(role) {
	case "system", "developer", "assistant":
		return true
	default:
		return false
	}
}

// Clean 清洗单条文本，返回改写后的文本与命中的规则名列表。
// 未命中或未启用时原样返回，不分配新字符串。
func Clean(text string, identity bool) (string, []string) {
	if text == "" || !identity {
		return text, nil
	}
	m := CurrentMode()
	if m == ModeOff {
		return text, nil
	}

	lower := strings.ToLower(text)
	var matched []rule
	for _, r := range rules {
		if r.aggressive && m != ModeAggressive {
			continue
		}
		for _, needle := range r.needles {
			if strings.Contains(lower, needle) {
				matched = append(matched, r)
				break
			}
		}
	}
	if len(matched) == 0 {
		return text, nil
	}

	out := text
	var hits []string
	for _, r := range matched {
		next := r.re.ReplaceAllString(out, r.replace)
		if next != out {
			hits = append(hits, r.name)
			out = next
		}
	}
	return out, hits
}

// CleanRoleTexts 返回清洗后的副本，不改动入参。
//
// 调用方必须继续用原始 rts 计算会话 fingerprint / 落库：清洗是有版本的
// （规则会变），若拿清洗结果做指纹，规则一升级就会把延续会话判成新会话。
func CleanRoleTexts(rts [][2]string) ([][2]string, map[string]int) {
	if CurrentMode() == ModeOff || len(rts) == 0 {
		return rts, nil
	}
	out := make([][2]string, len(rts))
	copy(out, rts)
	stats := make(map[string]int)
	for i := range out {
		cleaned, hits := Clean(out[i][1], identityRole(out[i][0]))
		if cleaned != out[i][1] {
			out[i][1] = cleaned
			for _, h := range hits {
				stats[h]++
			}
		}
	}
	return out, stats
}
