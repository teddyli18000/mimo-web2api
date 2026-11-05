package router

import "testing"

func TestClaudeAlias(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"claude-opus-4-6", ModelV26Pro},
		{"claude-3-opus-20240229", ModelV26Pro},
		{"claude-sonnet-4-6", ModelV26Flash},
		{"claude-3-5-sonnet", ModelV26Flash},
		{"claude-haiku-4-5", ModelV26Flash},
		{"claude-unknown", ""},
		{"mimo-v2.6-pro", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := claudeAlias(c.in); got != c.want {
			t.Errorf("claudeAlias(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRouteModelFallsBackForUnknownClaude(t *testing.T) {
	got := RouteModel("claude-unknown", "mimo-v2.6-pro")
	if got.Model != "mimo-v2.6-pro" {
		t.Fatalf("unknown claude model should fall back to default, got %q", got.Model)
	}
}

func TestRouteModelKeepsExplicitMiMo(t *testing.T) {
	got := RouteModel("mimo-v2.6-flash", "mimo-v2.6-pro")
	if got.Model != "mimo-v2.6-flash" {
		t.Fatalf("explicit model must be respected, got %q", got.Model)
	}
}
