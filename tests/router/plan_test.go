package router_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Polqt/spillway/internal/providers"
	"github.com/Polqt/spillway/internal/router"
)

func ref(t *testing.T, s string) providers.ModelRef {
	t.Helper()
	r, err := providers.ParseModelRef(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func refs(t *testing.T, ss ...string) []providers.ModelRef {
	t.Helper()
	out := make([]providers.ModelRef, len(ss))
	for i, s := range ss {
		out[i] = ref(t, s)
	}
	return out
}

const (
	textBody   = `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`
	toolsBody  = `{"model":"auto","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`
	visionBody = `{"model":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"u"}}]}]}`
)

// newRouter builds free = [a undeclared, b json_mode, c tools] and
// smart = [d tools+vision, c] with auto = [free, smart].
func newRouter(t *testing.T) *router.Router {
	t.Helper()
	r, err := router.New(router.Options{
		Tiers: map[string][]providers.ModelRef{
			"free":  refs(t, "p/a", "p/b", "p/c"),
			"smart": refs(t, "q/d", "p/c"),
		},
		Auto: []string{"free", "smart"},
		Declarations: map[providers.ModelRef]router.Declaration{
			ref(t, "p/b"): {Capabilities: []router.Capability{router.JSONMode}},
			ref(t, "p/c"): {Capabilities: []router.Capability{router.Tools}},
			ref(t, "q/d"): {Capabilities: []router.Capability{router.Tools, router.Vision}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func describe(p router.Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s needs=%v eligible=", p.Source, p.Needs)
	for i, model := range p.Eligible {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(p.Tiers[i] + ":" + model.String())
	}
	b.WriteString(" skipped=")
	for i, s := range p.Skipped {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(s.Ref.String() + " " + s.Reason)
	}
	return b.String()
}

func TestPlan_Selection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, model, body, want string
	}{
		{"auto without requirements reaches undeclared models", "auto", textBody,
			"auto needs=[] eligible=free:p/a,free:p/b,free:p/c,smart:q/d skipped="},
		{"auto with requirement skips undeclared", "auto", toolsBody,
			"auto needs=[tools] eligible=free:p/c,smart:q/d skipped=p/a capabilities not declared; p/b lacks tools"},
		{"auto vision", "auto", visionBody,
			"auto needs=[vision] eligible=smart:q/d skipped=p/a capabilities not declared; p/b lacks vision; p/c lacks vision"},
		{"tier tries undeclared models", "spillway/free", toolsBody,
			"tier needs=[tools] eligible=free:p/a,free:p/c skipped=p/b lacks tools"},
		{"tier without requirements", "spillway/smart", textBody,
			"tier needs=[] eligible=smart:q/d,smart:p/c skipped="},
	}
	r := newRouter(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plan, ok, err := r.Plan(tt.model, []byte(tt.body))
			if err != nil || !ok {
				t.Fatalf("Plan = %v, %v", ok, err)
			}
			if got := describe(plan); got != tt.want {
				t.Errorf("plan\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestNew_CopiesDifficultyRuleConditions(t *testing.T) {
	t.Parallel()

	tools := true
	r, err := router.New(router.Options{
		Tiers: map[string][]providers.ModelRef{"free": refs(t, "p/free")},
		Auto:  []string{"free"},
		Declarations: map[providers.ModelRef]router.Declaration{
			ref(t, "p/free"): {Capabilities: []router.Capability{router.Tools}},
		},
		DifficultyTiers: map[string][]string{
			"simple": {"free"}, "moderate": {"free"}, "hard": {"free"},
		},
		Rules: []router.Rule{{ToolsPresent: &tools, Difficulty: "hard"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tools = false
	plan, ok, err := r.Plan("auto", []byte(toolsBody))
	if err != nil || !ok {
		t.Fatalf("Plan = %v, %v", ok, err)
	}
	if plan.Rule != 1 || plan.Difficulty != "hard" {
		t.Errorf("Plan = rule %d difficulty %q, want rule 1 difficulty hard", plan.Rule, plan.Difficulty)
	}
}

func TestPlan_LacksListsOnlyMissingCapabilities(t *testing.T) {
	t.Parallel()
	r, err := router.New(router.Options{
		Tiers:        map[string][]providers.ModelRef{"free": refs(t, "p/a")},
		Declarations: map[providers.ModelRef]router.Declaration{ref(t, "p/a"): {Capabilities: []router.Capability{router.Tools}}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"u"}}]}],"tools":[{}],"response_format":{"type":"json_object"}}`
	_, _, err = r.Plan("spillway/free", []byte(body))
	if !errors.Is(err, router.ErrNoCapableModel) {
		t.Fatalf("Plan error = %v, want ErrNoCapableModel", err)
	}
	want := `No model in tier "free" supports this request, which needs tools, vision, json_mode: p/a lacks vision, json_mode. Declare capabilities under models: (ADR 0007).`
	if err.Error() != want {
		t.Errorf("Plan error\n got %s\nwant %s", err, want)
	}
}

func TestPlan_ContextWindow(t *testing.T) {
	t.Parallel()
	r, err := router.New(router.Options{
		Tiers: map[string][]providers.ModelRef{"free": refs(t, "p/small", "p/open", "p/large")},
		Auto:  []string{"free"},
		Declarations: map[providers.ModelRef]router.Declaration{
			ref(t, "p/small"): {ContextWindow: 32768},
			ref(t, "p/large"): {ContextWindow: 1 << 20},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	big := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 300_000) + `"}]}`
	plan, _, err := r.Plan("auto", []byte(big))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := "auto needs=[] eligible=free:p/open,free:p/large skipped=p/small context too small (est. 50000 > 32768)"
	if got := describe(plan); got != want {
		t.Errorf("plan\n got %s\nwant %s", got, want)
	}
	fits := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 100_000) + `"}]}`
	plan, _, err = r.Plan("auto", []byte(fits))
	if err != nil || len(plan.Eligible) != 3 {
		t.Errorf("Plan(100 KB) = %s, %v; want all three eligible", describe(plan), err)
	}
}

func TestPlan_AllTooSmallIsClassifiedAsContextLengthExceeded(t *testing.T) {
	t.Parallel()
	r, err := router.New(router.Options{
		Tiers:        map[string][]providers.ModelRef{"free": refs(t, "p/small")},
		Auto:         []string{"free"},
		Declarations: map[providers.ModelRef]router.Declaration{ref(t, "p/small"): {ContextWindow: 100}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = r.Plan("auto", []byte(`{"messages":[{"role":"user","content":"`+strings.Repeat("a", 6000)+`"}]}`))
	if !errors.Is(err, router.ErrNoCapableModel) {
		t.Fatalf("Plan error = %v, want ErrNoCapableModel", err)
	}
	var noCapable *router.NoCapableModelError
	if !errors.As(err, &noCapable) || !noCapable.EverySkipWasSize {
		t.Fatalf("Plan error = %#v, want all skips classified as context size", err)
	}
	want := `No model for "auto" has a context window large enough for this request (est. 1000 tokens): p/small context too small (est. 1000 > 100).`
	if err.Error() != want {
		t.Errorf("Plan error\n got %s\nwant %s", err, want)
	}
}

func TestPlan_NoCapableModelMixedReasons(t *testing.T) {
	t.Parallel()
	r := newRouter(t)
	body := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"u"}}]}],"response_format":{"type":"json_schema"}}`
	_, _, err := r.Plan("auto", []byte(body))
	if !errors.Is(err, router.ErrNoCapableModel) {
		t.Fatalf("Plan error = %v, want ErrNoCapableModel", err)
	}
	want := `No model for "auto" supports this request, which needs vision, structured_outputs: p/a capabilities not declared; p/b lacks vision, structured_outputs; p/c lacks vision, structured_outputs; q/d lacks structured_outputs. Declare capabilities under models: (ADR 0007).`
	if err.Error() != want {
		t.Errorf("Plan error\n got %s\nwant %s", err, want)
	}
}

func TestPlan_ResolutionErrors(t *testing.T) {
	t.Parallel()
	r := newRouter(t)
	_, _, err := r.Plan("spillway/missing", []byte(textBody))
	if !errors.Is(err, router.ErrUnknownTier) || err.Error() != `Tier "missing" not found. Configured tiers: free, smart.` {
		t.Errorf("unknown tier error = %v", err)
	}
	_, _, err = r.Plan("auto", []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":"x"}`))
	if !errors.Is(err, router.ErrInvalidField) {
		t.Errorf("invalid field error = %v", err)
	}
	unconfigured, err := router.New(router.Options{Tiers: map[string][]providers.ModelRef{"free": refs(t, "p/a")}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = unconfigured.Plan("auto", []byte(textBody))
	if !errors.Is(err, router.ErrAutoNotConfigured) || !strings.Contains(err.Error(), "router.auto") {
		t.Errorf("auto not configured error = %v", err)
	}
}

func TestPlan_DirectReferencesAreNotVirtual(t *testing.T) {
	t.Parallel()
	r := newRouter(t)
	for _, model := range []string{"p/a", "groq/llama", "Auto", "spillwayx/free"} {
		// A wrong-type field must not matter: direct bodies are forwarded untouched.
		_, ok, err := r.Plan(model, []byte(`{"messages":[{"role":"user","content":"hi"}],"tools":"x"}`))
		if ok || err != nil {
			t.Errorf("Plan(%q) = %v, %v; want not virtual", model, ok, err)
		}
	}
}

func TestPlan_SharedModelAppearsOnceAtFirstPosition(t *testing.T) {
	t.Parallel()
	r := newRouter(t)
	plan, _, err := r.Plan("auto", []byte(toolsBody))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	count := 0
	for i, model := range plan.Eligible {
		if model == ref(t, "p/c") {
			count++
			if plan.Tiers[i] != "free" {
				t.Errorf("p/c tier = %q, want free", plan.Tiers[i])
			}
		}
	}
	if count != 1 {
		t.Errorf("p/c appears %d times, want 1", count)
	}
}

func TestPlan_Deterministic(t *testing.T) {
	t.Parallel()
	r := newRouter(t)
	first, _, _ := r.Plan("auto", []byte(toolsBody))
	for range 100 {
		plan, _, _ := r.Plan("auto", []byte(toolsBody))
		if describe(plan) != describe(first) {
			t.Fatalf("plan changed: %s then %s", describe(first), describe(plan))
		}
	}
}

func TestRouter_VirtualModels(t *testing.T) {
	t.Parallel()
	got := strings.Join(newRouter(t).VirtualModels(), ",")
	if got != "auto,spillway/free,spillway/smart" {
		t.Errorf("VirtualModels = %s", got)
	}
}

func TestNew_RejectsInconsistentOptions(t *testing.T) {
	t.Parallel()
	tests := []router.Options{
		{Tiers: map[string][]providers.ModelRef{"free": refs(t, "p/a")}, Auto: []string{"nope"}},
		{Tiers: map[string][]providers.ModelRef{"free": refs(t, "p/a")}, Declarations: map[providers.ModelRef]router.Declaration{ref(t, "p/a"): {Capabilities: []router.Capability{"audio"}}}},
	}
	for i, o := range tests {
		t.Run(fmt.Sprintf("case %d", i), func(t *testing.T) {
			if _, err := router.New(o); err == nil {
				t.Error("New succeeded, want error")
			}
		})
	}
}
