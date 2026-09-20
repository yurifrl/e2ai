-- 0004_agents.sql — Phase 3 T1: agent session orchestrator schema.
-- agent_sessions: one row per run x role session (the agentic loop's state
-- of record; agents are ephemeral processes — ARCHITECTURE Pattern 1).
-- session_steps: append-only trajectory (SAFE-01 evidence trail).
--
-- Shape per RESEARCH.md §DDL sketch. Roles are the Phase 3 roster
-- (AGNT-01); the status vocabulary is the guarded state machine in
-- internal/agents/sessions.go — no caller writes agent_sessions.status
-- directly. Budget columns (BUDG-01) live on the session: steps_used and
-- cost_millicents are the accumulators, *_ceiling are the hard stops the
-- budget engine (internal/agents/budget.go) enforces inside the step
-- append transaction.

create table agent_sessions (
  id uuid primary key default gen_random_uuid(),
  run_id uuid not null references runs(id) on delete cascade,
  role text not null check (role in
    ('navigator','vision_judge','plan_author','edge_hunter','flake_analyst')),
  status text not null default 'queued' check (status in
    ('queued','claimed','running','completed','failed','killed','budget_exhausted')),
  worker_id text,
  heartbeat_at timestamptz,               -- staleness drives orphan sweep + stuck flag
  claimed_at timestamptz, started_at timestamptz, finished_at timestamptz,
  error_note text,                        -- infra failure / orphaned detail
  steps_used int not null default 0,
  cost_millicents bigint not null default 0,
  step_ceiling int not null default 0,            -- 0 = unlimited
  cost_ceiling_millicents bigint not null default 0, -- 0 = unlimited
  created_at timestamptz not null default now()
);
-- Claim scan: next queued session, oldest first (claim.go).
create index agent_sessions_claim_idx on agent_sessions (status, created_at);
-- Run detail / trajectory queries (report, REPT-04 groundwork).
create index agent_sessions_run_idx on agent_sessions (run_id, created_at);

create table session_steps (
  id uuid primary key default gen_random_uuid(),
  session_id uuid not null references agent_sessions(id) on delete cascade,
  idx int not null,                       -- per-session sequence, gapless
  kind text not null check (kind in
    ('navigate','act','observe','judge','assert','error')),
  decision jsonb not null default '{}',   -- what the agent decided
  action jsonb not null default '{}',     -- what it did about it
  evidence jsonb not null default '[]',   -- artifact refs (screenshots, a11y)
  tokens_in int not null default 0,
  tokens_out int not null default 0,
  cost_millicents bigint not null default 0,
  created_at timestamptz not null default now(),
  unique (session_id, idx)                -- append-only, gapless trajectory
);
