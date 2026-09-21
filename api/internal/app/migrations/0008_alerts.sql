-- 0008_alerts.sql — Phase 5 T4 (05-01-PLAN): TREN-01 alert events.
--
-- alert_events: durable alert records written by the flake-analyst trigger
-- (internal/agents/analyst.go → internal/flakes alerts store) and read by
-- the trends surface (GET /api/trends/alerts). Report-only posture: alerts
-- are an admin-visible flag + log line, never a notification or a gate.
-- kind vocabulary is closed so future threshold kinds (flip_rate) must land
-- with a migration, not a string typo. Dedupe/cooldown is enforced by the
-- writer (24h window, flakes.AlertCooldownHours) so a persistently crossed
-- threshold cannot spam the queue.
create table alert_events (
  id uuid primary key default gen_random_uuid(),
  app_id uuid not null references registered_apps(id),
  kind text not null check (kind in ('flake_threshold', 'flip_rate')),
  payload jsonb not null default '{}',   -- threshold, observed rate, worst test
  acknowledged boolean not null default false,
  created_at timestamptz not null default now()
);
create index alert_events_app_idx on alert_events (app_id, created_at desc);
