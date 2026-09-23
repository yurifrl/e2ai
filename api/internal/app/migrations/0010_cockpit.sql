-- 0010_cockpit.sql — Phase 6 wave 1 (06-01-PLAN T1): the complete Phase 6
-- control-plane substrate, minted once — later plans consume these objects,
-- never edit this file. Contents: runs.trigger_source widens to manual|
-- simulate (drop-and-re-add per 0007); runs.triggered_by + cockpit_run_ids
-- sequence + runs_one_manual_in_flight_per_app insert-as-gate index (the
-- manual trigger path, Plan 02); apps.budget_cap_millicents (the layered
-- budget cap, Plan 02's gate); audit_events is the attribution substrate
-- every Phase 6 mutation writes in the same transaction as its domain write
-- (AUDT-01); cockpit_state is the single-row arm state of record (COCK-04)
-- the pause control renders and later actuating boundaries (manual trigger,
-- webhook simulate, kill switch) consult.
--
-- runs.trigger_source
-- vocabulary is fixed now so the check constraint is written once.

alter table runs drop constraint runs_trigger_source_check;
alter table runs add constraint runs_trigger_source_check
  check (trigger_source in ('webhook', 'reconcile', 'sweep', 'manual', 'simulate'));

-- runs.triggered_by (T-06-19): operator attribution for runs born outside the
-- webhook. Nullable — webhook/reconcile/sweep runs have no operator; the
-- trigger path (Plan 02) sets it explicitly alongside trigger_source='manual'
-- so a DB-default ('webhook') misattribution is structurally impossible.
alter table runs add column triggered_by text;

-- cockpit_run_ids (TRIG-01): dedicated sequence for synthetic manual-run
-- identity. The trigger path negates nextval so synthetic ids are negative —
-- they can never collide with real GitHub ids under the
-- unique (app_id, workflow_run_id) webhook-dedupe key.
create sequence cockpit_run_ids;

-- Insert-as-gate (RESEARCH D1/P3): at most one in-flight manual run per app.
-- The trigger path inserts with on conflict do nothing — a racing second
-- trigger loses here and returns 409, which is the only mechanism that
-- serializes two rapid triggers to exactly one 202 + one 409 (their synthetic
-- ids differ, so River UniqueOpts alone cannot). In-flight = not terminal
-- (runs/state.go sets finished_at for exactly these two states).
create unique index runs_one_manual_in_flight_per_app on runs (app_id)
  where trigger_source = 'manual' and status not in ('completed', 'failed');

-- apps.budget_cap_millicents (BUDG-03): the nullable per-app cap on agent
-- spend (millicents, agent_sessions accumulators). Null = no per-app cap; the
-- global default env floor (E2AI_DEFAULT_BUDGET_CAP_MILLICENTS) applies
-- instead. Editable later via the P9 registry wizard — the column lands now
-- so Plan 02's gate reads one stable location.
alter table registered_apps add column budget_cap_millicents bigint;

-- audit_events (AUDT-01): one row per attributed cockpit mutation. Append-only
-- — updates and deletes are a schema violation, not an application convention.
-- actor_email is the BFF-resolved operator identity; the HTTP boundary refuses
-- placeholder identities before any write, so an actorless audit row can never
-- exist.
create table audit_events (
  id uuid primary key default gen_random_uuid(),
  actor_email text not null,
  action text not null check (action in ('trigger', 'simulate', 'pause', 'unpause')),
  target_type text not null,
  target_id text not null,
  reason text,
  details jsonb not null default '{}',
  created_at timestamptz not null default now()
);

create index audit_events_target_idx on audit_events (target_type, target_id, created_at desc);

-- cockpit_state (COCK-04): the arm's paused flag as a single-row table, not
-- client state. The boolean primary key doubles as the single-row guard —
-- every write upserts id=true, a second row is structurally impossible.
create table cockpit_state (
  id boolean primary key default true check (id),
  paused boolean not null default false,
  paused_reason text,
  paused_by text,
  paused_at timestamptz,
  updated_at timestamptz not null default now()
);

insert into cockpit_state (id) values (true);
