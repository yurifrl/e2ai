# Plan format

> The declarative contract for e2ai test plans. Locked by sketch-006
> (`.planning/sketches/006-plan-format/`); implemented by
> `api/internal/plans` (`Parse`, `Validate`, `ContentHash`). PLAN-01.

A test plan is a **markdown file**. Humans read and approve it; agents
execute it. Frontmatter tells the scheduler when and where to run; numbered
steps in the body tell the Navigator what to drive and the Vision Judge what
to accept.

```markdown
---
id: checkout-flow-v1.0.0
app: nsxbet-web
title: Checkout flow stays green
surfaces: [desktop-chrome, mobile-safari]
trigger: on-deploy
pr-filter:
  paths: ["**/checkout/**"]
timeout: 10m
derived: false
sources: []
---

## Steps

1. Open the checkout page as a logged-in user.
   judge: the balance widget is visible
2. Bet with a stake of 10.
   judge: the potential win shows at least 20
   uses: MATCH-LIVE-BADGE
   on-failure: continue
```

## Frontmatter

The YAML document between a leading `---` and a closing `---` fence. The key
set is **closed** — an unknown key is a parse error, so a typo fails at
ingest instead of silently scheduling nothing.

| Key | Rule |
| --- | --- |
| `id` | Required. `^[a-z0-9][a-z0-9-]*-v\d+\.\d+\.\d+$` — stable slug + full semver suffix. Renaming the slug is a new plan; bumping the suffix is a new version. |
| `app` | Required. Must resolve to a registered app (registry lookup at validation). |
| `title` | Required, non-empty. Human-readable summary for the approval queue. |
| `surfaces` | Required, at least one. Every entry must be a surface of `app`. |
| `trigger` | Required. One of `on-deploy`, `on-demand`, `nightly`. |
| `pr-filter.paths` | Optional, only meaningful with `on-deploy`. Glob list (gobwas/glob syntax, `**` supported). Restricts the plan to deploys whose PR touches a matching path. Present means `paths` must be non-empty; every glob must compile. |
| `timeout` | Optional Go duration string (`10m`), must be positive. Default: `10m` per surface. |
| `derived` | Optional bool. Marks a machine-drafted plan (DERV-02). |
| `sources` | Optional list of provenance IDs; non-empty strings. Required-in-spirit when `derived: true` — see the tautology guard. |

## Body

- Lines starting with `#`–`######` are markdown comments — ignored by the
  parser, free for humans.
- A line matching `N. text` **opens step N**. Numbers may be sparse (gaps
  allowed) but must strictly increase.
- Inside a step, indented or following plain lines are continuation lines
  that extend the last-open field. A blank line closes the field.
- Step fields:

  | Field | Rule |
  | --- | --- |
  | `judge:` | Falsifiable acceptance check for the Vision Judge. **Required.** Must carry at least one checkable predicate: a numeric bound (`at least 20`, `2.40`), a state-assertion verb (`is visible`, `shows`, `contains`, `remains disabled`, …), or a `uses:` reference. Agent-plausible but unverifiable phrasing ("the flow works nicely") is rejected at parse time. |
  | `intent:` / opening text | The Navigator's driving instruction — plain language, no selectors. |
  | `uses:` | Comma-separated corpus behavior keys (`^[A-Z0-9][A-Z0-9-]*$`, e.g. `MATCH-LIVE-BADGE`). Each key must resolve to a **verified** corpus behavior at validation. |
  | `on-failure:` | One of `stop` (default), `continue`, `diagnose`. |

- A plan body with **no numbered steps** is a parse error.

## Content-hash identity (APPR-01)

A plan's version identity is `sha256` over **normalized** content: CRLF→LF,
trailing whitespace stripped per line, trailing blank lines removed —
everything else verbatim. Editors and OSes cannot change an approved
version; any other byte change (a word in a judge, a frontmatter path) is a
new version. The hash prefixes the stored plan id in the approvals flow.

## Validation (needs lookups — `Validate`)

Beyond parse rules, ingest validates against the database through two
narrow interfaces so validation stays pure:

1. `app` exists; every `surfaces` entry is one of its surfaces.
2. Every `uses:` key exists **and** is `verified` in the corpus
   (CORP-03: only verified behaviors may be used).
3. **Tautology guard (DERV-03):** a `derived` plan must have non-empty
   `sources` with at least one non-diff-derived source. A plan drafted
   purely from the diff it will later judge would approve itself.

## Derivation lint (`LintDiffTokens`)

When deriving plans from a PR diff (T4), `DiffIdentifierTokens` extracts
identifier-shaped tokens (camelCase, snake_case, api paths, numbers) from
the diff, and `LintDiffTokens` flags any derived step judge that names a
token — a fresh judge referencing freshly changed identifiers is a tautology
risk. Empty token list = nothing to check.
