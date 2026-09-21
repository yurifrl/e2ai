-- 0007_hardening.sql — Phase 5 T1 (05-01-PLAN): flake governance + webhook
-- triage + per-app edge-sweep config.
--
-- test_quarantine (FLAK-01/02): the quarantine store. One row per test
-- identity (app_id, file, title); activate re-opens a lifted row via the
-- guarded upsert in internal/flakes, so an active quarantine can never be
-- silently duplicated or double-activated. file is NOT NULL default '' —
-- Postgres treats NULLs as distinct in unique constraints, which would break
-- the one-row-per-identity invariant for title-only identities.
-- Status transitions: active → lifted only (activate/lift/expire in
-- internal/flakes/quarantine.go); quarantined_by is agent|human — agent rows
-- are proposals the report-only rule turns into skips, never app mutations.
create table test_quarantine (
  id uuid primary key default gen_random_uuid(),
  app_id uuid not null references registered_apps(id),
  file text not null default '',         -- test identity half: spec file
  title text not null,                   -- test identity half: test title
  reason text not null,
  evidence_artifact text,                -- artifact ref backing the decision
  status text not null default 'active' check (status in ('active','lifted')),
  quarantined_by text not null check (quarantined_by in ('agent','human')),
  created_at timestamptz not null default now(),
  expires_at timestamptz,                -- null = no expiry; Expire lifts past rows
  lifted_at timestamptz,
  lifted_by text,                        -- actor | 'system' when expired
  unique (app_id, file, title)
);
create index test_quarantine_status_idx on test_quarantine (app_id, status);

-- webhook_triage (APPS-03): signature-valid deliveries no registered app
-- claims. Admin triage queue, not a crash path — the delivery still gets a
-- 202, the evidence lands here. Dedupe on delivery_id keeps redeliveries
-- from spamming the queue.
create table webhook_triage (
  id uuid primary key default gen_random_uuid(),
  delivery_id text not null,             -- X-GitHub-Delivery
  workflow_name text not null default '',
  display_title text not null default '',
  workflow_run_id bigint,
  payload jsonb not null default '{}',   -- verbatim body, evidence trail
  signature_valid boolean not null default true,
  reason text not null,                  -- e.g. unknown-app
  created_at timestamptz not null default now(),
  last_seen_at timestamptz not null default now(), -- bumped on redelivery
  occurrences int not null default 1,    -- redelivery counter
  unique (delivery_id)
);
create index webhook_triage_created_idx on webhook_triage (created_at desc);

-- registered_apps additions (EDGE-01, TREN-01): per-app edge-sweep opt-in
-- (DB config, not env — RESEARCH §Pattern) and the flake alert threshold the
-- UI/future alerts compare flake rates against.
alter table registered_apps
  add column edge_sweep_enabled boolean not null default false,
  add column edge_sweep_interval_minutes int,
  add column edge_sweep_step_ceiling int,
  add column flake_alert_threshold numeric;

-- runs.trigger_source gains 'sweep' (EDGE-01 sweep-triggered runs). 0001 has
-- only a comment, no check constraint — this adds the real one, closing the
-- unvalidated-value hole while widening the vocabulary.
alter table runs add constraint runs_trigger_source_check
  check (trigger_source in ('webhook', 'reconcile', 'sweep'));
