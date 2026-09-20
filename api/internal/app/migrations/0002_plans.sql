-- 0002_plans.sql — plans, approvals, and corpus schema (Phase 2, T1).
-- Verbatim from RESEARCH.md §Pattern 4. New tables only: registered_apps
-- already reserves plans_source/corpus_root (0001) and is NOT altered here;
-- River's own tables come from rivermigrate and stay untouched.

create table plans (
  id text primary key,                       -- frontmatter id, stable slug
  app_id uuid not null references registered_apps(id),
  source_path text not null,
  status text not null,                      -- draft|pending|approved|rejected (max over versions)
  latest_version_id uuid,
  updated_at timestamptz not null default now()
);
create table plan_versions (
  id uuid primary key default gen_random_uuid(),
  plan_id text not null references plans(id),
  version text not null,                     -- semver from frontmatter id
  content_hash text not null,
  content text not null,                     -- verbatim markdown
  frontmatter jsonb not null,                -- parsed + validated
  status text not null,                      -- draft|pending_approval|approved|rejected|superseded
  sources jsonb not null default '[]',       -- tautology-guard provenance IDs
  created_by text not null,
  created_at timestamptz not null default now(),
  unique (plan_id, content_hash)
);
create table plan_approvals (
  id uuid primary key default gen_random_uuid(),
  plan_version_id uuid not null references plan_versions(id),
  decision text not null check (decision in ('approved','rejected')),
  approver text not null,                    -- httpx UserOf; never 'you' in prod
  notes text,
  decided_at timestamptz not null default now(),
  unique (plan_version_id, decision)         -- one decision per version
);
create table corpus_behaviors (
  id uuid primary key default gen_random_uuid(),
  app_id uuid not null references registered_apps(id),
  key text not null,                         -- e.g. MATCH-LIVE-BADGE
  title text not null,
  description text not null,                 -- falsifiable expected behavior
  status text not null default 'unverified' check (status in ('unverified','verified','stale')),
  provenance jsonb not null,                 -- {kind: playwright-spec|run|learning, path/run_id/learning_id, commit}
  created_at timestamptz not null default now(),
  verified_by text, verified_at timestamptz,
  unique (app_id, key)
);
create table corpus_learnings (
  id uuid primary key default gen_random_uuid(),
  app_id uuid not null references registered_apps(id),
  run_id uuid references runs(id),
  observation text not null,
  status text not null default 'unverified' check (status in ('unverified','promoted','dismissed')),
  created_at timestamptz not null default now()
);
