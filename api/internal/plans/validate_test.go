package plans

import (
	"reflect"
	"strings"
	"testing"
)

// fakeApps satisfies AppLookup from an in-memory map.
type fakeApps map[string][]string

func (f fakeApps) LookupApp(ref string) ([]string, bool) {
	s, ok := f[ref]
	return s, ok
}

// fakeBehaviors satisfies BehaviorStatus from an in-memory map.
type fakeBehaviors map[string]string

func (f fakeBehaviors) BehaviorStatus(key string) (string, bool) {
	s, ok := f[key]
	return s, ok
}

var (
	apps      = fakeApps{"bet-app": {"web", "mobile"}, "other-app": {"desktop"}}
	behaviors = fakeBehaviors{"MATCH-LIVE-BADGE": "verified", "PWSPEC-ODDS": "unverified"}
)

// minimalPlan is a plan that passes Validate against the fixtures above.
const minimalPlan = `---
id: valid-v1.0.0
app: bet-app
title: t
surfaces: [web]
trigger: on-demand
---
1. Open the page.
   judge: the badge is visible
`

func TestValidatePasses(t *testing.T) {
	p := mustParse(t, minimalPlan)
	if err := Validate(p, apps, behaviors); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	planWith := func(mutate func(fm *Frontmatter, steps *[]Step)) *Plan {
		p := mustParse(t, minimalPlan)
		mutate(&p.Frontmatter, &p.Steps)
		return p
	}
	badJudge := func(j string) Step {
		return Step{Number: 1, Intent: "Open the page.", Judge: j, OnFailure: OnFailureStop}
	}
	cases := []struct {
		name    string
		plan    *Plan
		wantErr string
	}{
		{
			name: "app not registered",
			plan: planWith(func(fm *Frontmatter, _ *[]Step) { fm.App = "ghost-app" }),
			wantErr: `app "ghost-app": not registered`,
		},
		{
			name: "surface not a subset",
			plan: planWith(func(fm *Frontmatter, _ *[]Step) { fm.Surfaces = []string{"web", "desktop"} }),
			wantErr: `surface "desktop": not a surface of app "bet-app"`,
		},
		{
			name: "malformed pr-filter glob",
			plan: planWith(func(fm *Frontmatter, _ *[]Step) {
				fm.PRFilter = &PRFilter{Paths: []string{"web/[unclosed"}}
			}),
			wantErr: `glob "web/[unclosed"`,
		},
		{
			name: "well-formed pr-filter glob compiles",
			plan: planWith(func(fm *Frontmatter, _ *[]Step) {
				fm.PRFilter = &PRFilter{Paths: []string{"web/**", "*.ts"}}
			}),
			wantErr: "__none__",
		},
		{
			name: "derived with no sources",
			plan: planWith(func(fm *Frontmatter, _ *[]Step) { fm.Derived = true; fm.Sources = nil }),
			wantErr: "derived plan: sources required",
		},
		{
			name: "derived with only diff sources (tautology)",
			plan: planWith(func(fm *Frontmatter, _ *[]Step) {
				fm.Derived = true
				fm.Sources = []string{"diff", "diff:PR-142"}
			}),
			wantErr: "tautological",
		},
		{
			name: "derived with one independent source passes",
			plan: planWith(func(fm *Frontmatter, _ *[]Step) {
				fm.Derived = true
				fm.Sources = []string{"diff:PR-142", "CORP-SEED-RUN-7"}
			}),
			wantErr: "__none__",
		},
		{
			name: "non-derived plan skips the tautology rule",
			plan: planWith(func(fm *Frontmatter, _ *[]Step) { fm.Sources = []string{"diff"} }),
			wantErr: "__none__",
		},
		{
			name: "step without judge",
			plan: planWith(func(_ *Frontmatter, steps *[]Step) {
				*steps = []Step{{Number: 1, Intent: "Open the page.", OnFailure: OnFailureStop}}
			}),
			wantErr: "step 1: judge required",
		},
		{
			name: "judge without falsifiable predicate",
			plan: planWith(func(_ *Frontmatter, steps *[]Step) { *steps = []Step{badJudge("the flow works nicely")} }),
			wantErr: "step 1: judge is not falsifiable",
		},
		{
			name: "judge falsifiable via state assertion",
			plan: planWith(func(_ *Frontmatter, steps *[]Step) { *steps = []Step{badJudge("the badge is visible")} }),
			wantErr: "__none__",
		},
		{
			name: "judge falsifiable via numeric bound",
			plan: planWith(func(_ *Frontmatter, steps *[]Step) { *steps = []Step{badJudge("at least 12 bets render")} }),
			wantErr: "__none__",
		},
		{
			name: "uses key missing from corpus",
			plan: planWith(func(_ *Frontmatter, steps *[]Step) {
				(*steps)[0].Uses = []string{"NOPE-MISSING"}
			}),
			wantErr: `uses "NOPE-MISSING": no such corpus behavior`,
		},
		{
			name: "uses key not verified",
			plan: planWith(func(_ *Frontmatter, steps *[]Step) {
				(*steps)[0].Uses = []string{"PWSPEC-ODDS"}
			}),
			wantErr: `behavior is "unverified", only verified behaviors may be used`,
		},
		{
			name: "uses verified key passes",
			plan: planWith(func(_ *Frontmatter, steps *[]Step) {
				(*steps)[0].Uses = []string{"MATCH-LIVE-BADGE"}
			}),
			wantErr: "__none__",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.plan, apps, behaviors)
			if tc.wantErr == "__none__" {
				if err != nil {
					t.Errorf("Validate: %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate: nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want containing %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestDiffIdentifierTokens(t *testing.T) {
	diff := `+func loadOddsBundle(user_profile *API) {
+	oldMethod := x.y.z
+	// prose: the request has no identifiers
+	var camelCase int
`
	tokens := DiffIdentifierTokens(diff)
	want := []string{"API", "camelCase", "loadOddsBundle", "oldMethod", "user_profile"}
	if !reflect.DeepEqual(tokens, want) {
		t.Errorf("tokens = %v, want %v (deduped, sorted, no prose)", tokens, want)
	}
}

func TestLintDiffTokens(t *testing.T) {
	plan := mustParse(t, `---
id: lint-v1.0.0
app: bet-app
title: t
surfaces: [web]
trigger: on-deploy
---
1. Open the odds page.
   judge: oddsBundle renders after load
2. Bet 10 units.
   judge: at least 12 open bets show
`)
	tokens := DiffIdentifierTokens("+const oddsBundle = 1\n+const unrelatedWidget = 2")
	findings := LintDiffTokens(plan, tokens)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Step != 1 || f.Token != "oddsBundle" || f.Judge != "oddsBundle renders after load" {
		t.Errorf("finding = %+v", f)
	}

	// A word that merely contains the token as a substring must not flag:
	// "odds" is not "oddsBundle".
	plan2 := mustParse(t, `---
id: lint2-v1.0.0
app: bet-app
title: t
surfaces: [web]
trigger: on-deploy
---
1. Open the odds page.
   judge: the odds list loads
`)
	if got := LintDiffTokens(plan2, []string{"oddsBundle"}); got != nil {
		t.Errorf("substring flags = %v, want none", got)
	}

	if got := LintDiffTokens(plan, nil); got != nil {
		t.Errorf("empty tokens = %v, want nil", got)
	}
}
