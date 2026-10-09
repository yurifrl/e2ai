-- 0001_init.sql — e2ai application schema (Wave 1, T3).
-- Verbatim from RESEARCH.md § Postgres schema. River's own tables are NOT here:
-- they come from River's bundled migrations (rivermigrate), which coexist
-- without conflict because they own only river_* names.

-- registry (APPS-01/02): config-as-code, seeded from registry/*.yaml on boot
create table registered_apps (
  id uuid primary key default gen_random_uuid(),
  owner text not null, repo text not null,
  workflow_name text not null,           -- "Release - Web"
  workflow_path text not null,           -- ".github/workflows/remote_workflow_betweb.yaml"
  environment text not null,             -- "staging"
  display_title_pattern text not null,   -- "\[BetWeb\] Deploy to staging"
  health_url text,                       -- rollout gate (INGE-03)
  surfaces jsonb not null default '["web"]',
  plans_source text, corpus_root text,   -- future phases (columns reserved)
  webhook_secret_ref text not null,      -- 1Password ref; plaintext never stored
  comment_marker text not null,          -- "<!-- e2ai:run:{slug} -->"
  created_at timestamptz not null default now(),
  unique (owner, repo, workflow_name, environment)
);

-- ingest dedupe (INGE-01)
create table webhook_events (
  id uuid primary key default gen_random_uuid(),
  app_id uuid not null references registered_apps(id),
  delivery_id text not null,             -- X-GitHub-Delivery
  workflow_run_id bigint not null,
  signature_valid boolean not null,
  payload jsonb not null,
  received_at timestamptz not null default now(),
  unique (app_id, delivery_id)
);

-- runs (pipeline state; SUIT-02 baseline-ready)
create table runs (
  id uuid primary key default gen_random_uuid(),
  app_id uuid not null references registered_apps(id),
  workflow_run_id bigint not null,
  run_number int not null,
  head_sha text not null, head_branch text,
  environment text not null,
  trigger_source text not null default 'webhook',  -- webhook | reconcile
  status text not null default 'queued',           -- queued|waiting-rollout|provisioning|executing|reporting|completed|failed (lowercase at rest; feed renders upper() per RUN_TONE contract)
  verdict text,          -- pass|fail|flaky|infra_error|inconclusive (set at report; null until then)
  pr_number int, pr_state text,
  total_tests int, passed int, failed int, flaky int, skipped int, duration_ms int,
  report_url text, comment_url text, check_run_id bigint,
  error_note text,       -- infra failure detail (VERD-01 evidence trail)
  started_at timestamptz, finished_at timestamptz,
  created_at timestamptz not null default now(),
  unique (app_id, workflow_run_id)
);
create index on runs (app_id, created_at desc);

-- per-test evidence (VERD-01/REPT-01)
create table test_results (
  id uuid primary key default gen_random_uuid(),
  run_id uuid not null references runs(id) on delete cascade,
  suite text, title text not null, file text, line int, project text,  -- project = viewport project name (PLAT-01)
  outcome text not null,     -- playwright JSON outcome: expected|unexpected|flaky|skipped
  status text not null,      -- last attempt status: passed|failed|timedOut|skipped|interrupted
  attempts int not null default 1,
  duration_ms int,
  error_message text,
  classification text,       -- app_bug|test_gap|agent_flake|infra (REPT-02 heuristics)
  attachments jsonb          -- [{kind: screenshot|trace|video, path}]
);
create index on test_results (run_id);

create table artifacts (
  id uuid primary key default gen_random_uuid(),
  run_id uuid not null references runs(id) on delete cascade,
  kind text not null,        -- html_report|blob|trace|screenshot|video|log|results_json
  path text not null,        -- volume-relative; served at /e2ai/artifacts/{path}
  size_bytes bigint,
  created_at timestamptz not null default now()
);

create table pr_comments (  -- sticky-comment state (REPT-03)
  id uuid primary key default gen_random_uuid(),
  app_id uuid not null references registered_apps(id),
  pr_number int not null,
  comment_id bigint not null,
  last_run_id uuid references runs(id),
  updated_at timestamptz not null default now(),
  unique (app_id, pr_number)
);
