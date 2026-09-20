package plans

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// fullPlan is the richest plan the format allows; most happy-path cases
// start from it (or a trimmed copy) so tests read like real artifacts.
const fullPlan = `---
id: checkout-flow-v3.0.0
app: bet-app
title: Checkout flow guards the balance
surfaces:
  - web
  - mobile
trigger: on-deploy
pr-filter:
  paths:
    - web/checkout/**
    - "web/cart/*.ts"
timeout: 20m
derived: true
sources:
  - diff:PR-142
  - CORP-SEED-RUN-7
---

## Steps

Introductory prose before the first step is ignored.

1. Open the checkout page as a logged-in user.
   judge: the balance widget is visible
2. Bet with a stake of 10
   judge: at least 12 open bets render
   uses: MATCH-LIVE-BADGE, PWSPEC-ODDS-WIDGET
   on-failure: continue

Trailing prose after a key field is not part of any step.
`

func mustParse(t *testing.T, src string) *Plan {
	t.Helper()
	p, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func TestParseFull(t *testing.T) {
	p := mustParse(t, fullPlan)
	fm := p.Frontmatter
	if fm.ID != "checkout-flow-v3.0.0" || fm.App != "bet-app" || fm.Title != "Checkout flow guards the balance" {
		t.Errorf("identity fields = %+v", fm)
	}
	if len(fm.Surfaces) != 2 || fm.Surfaces[0] != "web" || fm.Surfaces[1] != "mobile" {
		t.Errorf("surfaces = %v", fm.Surfaces)
	}
	if fm.Trigger != TriggerOnDeploy {
		t.Errorf("trigger = %q", fm.Trigger)
	}
	if fm.PRFilter == nil || len(fm.PRFilter.Paths) != 2 || fm.PRFilter.Paths[0] != "web/checkout/**" {
		t.Errorf("pr-filter = %+v", fm.PRFilter)
	}
	if fm.Timeout != Duration(20*time.Minute) {
		t.Errorf("timeout = %v", fm.Timeout.D())
	}
	if !fm.Derived || len(fm.Sources) != 2 || fm.Sources[0] != "diff:PR-142" {
		t.Errorf("derived/sources = %v %v", fm.Derived, fm.Sources)
	}

	if len(p.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(p.Steps))
	}
	s1, s2 := p.Steps[0], p.Steps[1]
	if s1.Number != 1 || s1.Intent != "Open the checkout page as a logged-in user." {
		t.Errorf("step 1 = %+v", s1)
	}
	if s1.Judge != "the balance widget is visible" || s1.OnFailure != OnFailureStop || s1.Uses != nil {
		t.Errorf("step 1 fields = %+v", s1)
	}
	if s2.Number != 2 {
		t.Errorf("step 2 number = %d", s2.Number)
	}
	// A numbered gap (1 then 5) is allowed; steps are authored sparsely.
	if s2.Intent != "Bet with a stake of 10" {
		t.Errorf("step 2 intent = %q", s2.Intent)
	}
	if s2.Judge != "at least 12 open bets render" {
		t.Errorf("step 2 judge = %q", s2.Judge)
	}
	if len(s2.Uses) != 2 || s2.Uses[0] != "MATCH-LIVE-BADGE" || s2.Uses[1] != "PWSPEC-ODDS-WIDGET" {
		t.Errorf("step 2 uses = %v", s2.Uses)
	}
	if s2.OnFailure != OnFailureContinue {
		t.Errorf("step 2 on-failure = %q", s2.OnFailure)
	}
}

func TestParseMultilineContinuation(t *testing.T) {
	src := `---
id: sess-v1.0.0
app: bet-app
title: t
surfaces: [web]
trigger: on-demand
---
1. Open the live page.
   judge: the badge
     is visible
   and colored
`
	p := mustParse(t, src)
	j := p.Steps[0].Judge
	if j != "the badge is visible and colored" {
		t.Errorf("judge continuation = %q", j)
	}
}

func TestParseContinuationStopsAtBlankLine(t *testing.T) {
	src := `---
id: sess-v1.0.0
app: bet-app
title: t
surfaces: [web]
trigger: on-demand
---
1. Open the live page.
   judge: the badge is visible

This prose after a blank line belongs to no step.
`
	p := mustParse(t, src)
	if got := p.Steps[0].Judge; got != "the badge is visible" {
		t.Errorf("judge = %q, continuation must not cross a blank line", got)
	}
}

func TestParseMinimalDefaults(t *testing.T) {
	src := `---
id: minimal-v1.0.0
app: bet-app
title: t
surfaces: [web]
trigger: nightly
---
1. Do the thing.
   judge: it is visible
`
	p := mustParse(t, src)
	fm := p.Frontmatter
	if fm.Derived || fm.Sources != nil || fm.PRFilter != nil {
		t.Errorf("defaults = derived:%v sources:%v pr-filter:%v", fm.Derived, fm.Sources, fm.PRFilter)
	}
	if fm.Timeout != Duration(DefaultTimeout) {
		t.Errorf("timeout default = %v, want %v", fm.Timeout.D(), DefaultTimeout)
	}
	s := p.Steps[0]
	if s.OnFailure != OnFailureStop || s.Judge != "it is visible" {
		t.Errorf("step = %+v", s)
	}
}

// TestParseRejects walks every closed-set rule the format fixes. Each case
// must fail with a message naming the offending rule (ingest surfaces these).
func TestParseRejects(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantErr string
	}{
		{
			name:    "missing opening fence",
			src:     "id: x\n",
			wantErr: "missing frontmatter",
		},
		{
			name:    "frontmatter never closed",
			src:     "---\nid: x-v1.0.0\napp: a\ntitle: t\nsurfaces: [web]\ntrigger: on-demand\n",
			wantErr: "frontmatter never closed",
		},
		{
			name: "unknown frontmatter key",
			src: `---
id: x-v1.0.0
app: a
title: t
surfaces: [web]
trigger: on-demand
bogus: 1
---
1. Step.
   judge: it is visible
`,
			wantErr: "unknown field",
		},
		{
			name: "bad trigger enum",
			src: `---
id: x-v1.0.0
app: a
title: t
surfaces: [web]
trigger: hourly
---
1. Step.
   judge: it is visible
`,
			wantErr: "must be one of on-deploy|on-demand|nightly",
		},
		{
			name: "id missing semver suffix",
			src: `---
id: checkout-flow
app: a
title: t
surfaces: [web]
trigger: on-demand
---
1. Step.
   judge: it is visible
`,
			wantErr: "must match",
		},
		{
			name: "id uppercase slug",
			src: `---
id: Checkout-Flow-v1.0.0
app: a
title: t
surfaces: [web]
trigger: on-demand
---
1. Step.
   judge: it is visible
`,
			wantErr: "must match",
		},
		{
			name: "missing title",
			src: `---
id: x-v1.0.0
app: a
surfaces: [web]
trigger: on-demand
---
1. Step.
   judge: it is visible
`,
			wantErr: "title: required",
		},
		{
			name: "no surfaces",
			src: `---
id: x-v1.0.0
app: a
title: t
surfaces: []
trigger: on-demand
---
1. Step.
   judge: it is visible
`,
			wantErr: "surfaces: required",
		},
		{
			name: "pr-filter with empty paths",
			src: `---
id: x-v1.0.0
app: a
title: t
surfaces: [web]
trigger: on-deploy
pr-filter:
  paths: []
---
1. Step.
   judge: it is visible
`,
			wantErr: "present but paths is empty",
		},
		{
			name: "unparseable timeout",
			src: `---
id: x-v1.0.0
app: a
title: t
surfaces: [web]
trigger: on-demand
timeout: soon
---
1. Step.
   judge: it is visible
`,
			wantErr: "invalid duration",
		},
		{
			name: "non-positive timeout",
			src: `---
id: x-v1.0.0
app: a
title: t
surfaces: [web]
trigger: on-demand
timeout: 0s
---
1. Step.
   judge: it is visible
`,
			wantErr: "timeout must be positive",
		},
		{
			name:    "empty frontmatter",
			src:     "---\n---\n1. Step.\n",
			wantErr: "frontmatter:",
		},
		{
			name: "no numbered steps",
			src: `---
id: x-v1.0.0
app: a
title: t
surfaces: [web]
trigger: on-demand
---
Just prose. No steps at all.
`,
			wantErr: "plan has no numbered steps",
		},
		{
			name: "step numbers must strictly increase (equal)",
			src: `---
id: x-v1.0.0
app: a
title: t
surfaces: [web]
trigger: on-demand
---
1. First.
   judge: it is visible
1. Second.
   judge: it is visible
`,
			wantErr: "must be greater than the previous step",
		},
		{
			name: "step numbers must strictly increase (decreasing)",
			src: `---
id: x-v1.0.0
app: a
title: t
surfaces: [web]
trigger: on-demand
---
2. First.
   judge: it is visible
1. Second.
   judge: it is visible
`,
			wantErr: "must be greater than the previous step",
		},
		{
			name: "key field before first step",
			src: `---
id: x-v1.0.0
app: a
title: t
surfaces: [web]
trigger: on-demand
---
judge: premature
1. Step.
   judge: it is visible
`,
			wantErr: "before the first numbered step",
		},
		{
			name: "uses with lowercase key",
			src: `---
id: x-v1.0.0
app: a
title: t
surfaces: [web]
trigger: on-demand
---
1. Step.
   judge: it is visible
   uses: match-live-badge
`,
			wantErr: `behavior key "match-live-badge"`,
		},
		{
			name: "uses empty value",
			src: `---
id: x-v1.0.0
app: a
title: t
surfaces: [web]
trigger: on-demand
---
1. Step.
   judge: it is visible
   uses:
`,
			wantErr: "reference is empty",
		},
		{
			name: "on-failure unknown value",
			src: `---
id: x-v1.0.0
app: a
title: t
surfaces: [web]
trigger: on-demand
---
1. Step.
   judge: it is visible
   on-failure: retry
`,
			wantErr: "must be one of continue|diagnose",
		},
		{
			name: "sources with empty entry",
			src: `---
id: x-v1.0.0
app: a
title: t
surfaces: [web]
trigger: on-demand
derived: true
sources:
  - " "
---
1. Step.
   judge: it is visible
`,
			wantErr: "empty provenance id",
		},
		{
			name: "yaml type error on surfaces",
			src: `---
id: x-v1.0.0
app: a
title: t
surfaces: web
trigger: on-demand
---
1. Step.
   judge: it is visible
`,
			wantErr: "frontmatter:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatalf("Parse succeeded, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want containing %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestContentHash pins the normalization contract (APPR-01): editors and
// OSes must not change a version's identity; any other byte change must.
func TestContentHash(t *testing.T) {
	base := "---\nid: x-v1.0.0\n---\n1. Step.\n   judge: it is visible\n"

	hash := func(b []byte) string {
		t.Helper()
		h := ContentHash(b)
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(h) {
			t.Fatalf("hash %q is not 64 hex chars", h)
		}
		return h
	}

	want := hash([]byte(base))

	crlf := strings.ReplaceAll(base, "\n", "\r\n")
	if got := hash([]byte(crlf)); got != want {
		t.Errorf("CRLF changed the identity: %s != %s", got, want)
	}

	trailing := strings.ReplaceAll(base, "visible", "visible   ")
	trailing = strings.ReplaceAll(trailing, "\n", " \t\n")
	if got := hash([]byte(trailing)); got != want {
		t.Errorf("trailing whitespace changed the identity: %s != %s", got, want)
	}

	if got := hash([]byte(base + "\n\n\n")); got != want {
		t.Errorf("trailing blank lines changed the identity: %s != %s", got, want)
	}

	changedBody := strings.Replace(base, "visible", "hidden", 1)
	if got := hash([]byte(changedBody)); got == want {
		t.Error("body change did not change the identity")
	}

	changedFm := strings.Replace(base, "x-v1.0.0", "x-v2", 1)
	if got := hash([]byte(changedFm)); got == want {
		t.Error("frontmatter change did not change the identity")
	}
}
