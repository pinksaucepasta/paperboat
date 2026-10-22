-- Realistic-scale seed for the performance check (internal/perf). Deterministic: ids derive from md5 of a
-- counter, so every run builds the same data. Runs once per database inside one transaction.
-- Actors used by the checks:
--   user_perf_0001  owner of team perf-team-001 (200 members, 1000 team machines)
--   user_perf_0002  solo user with 100 personal machines, many terminals, previews and tunnels
--   user_perf_0003  member of perf-team-001 with selected-member grants
SET LOCAL search_path = paperboat;
-- Bulk rows are consistent by construction; row triggers (machine event fan-out, alias guards) would make the
-- seed quadratic. The checks themselves run with triggers on.
SET LOCAL session_replication_role = replica;

CREATE FUNCTION pg_temp.uuid4(seed text) RETURNS text LANGUAGE sql IMMUTABLE AS $$
  SELECT substr(h, 1, 8) || '-' || substr(h, 9, 4) || '-4' || substr(h, 14, 3) || '-8' || substr(h, 18, 3) || '-' ||
         substr(h, 21, 12) FROM md5(seed) h $$;
CREATE FUNCTION pg_temp.pid(prefix text, n bigint) RETURNS text LANGUAGE sql IMMUTABLE AS
$$ SELECT prefix || '_' || pg_temp.uuid4(prefix || n) $$;

-- 1200 accounts.
INSERT INTO users(id, workos_subject, primary_email, display_name, status, email_verified, canonical_email, created_at)
SELECT 'user_perf_' || lpad(i::text, 4, '0'), 'user_perf_subject_' || i, 'perf' || i || '@example.test',
       'Perf User ' || i, 'active', true, 'perf' || i || '@example.test', now() - (i || ' hours')::interval
FROM generate_series(1, 1200) i;

-- 100 teams: team 1 has 200 members, the rest 2..21.
INSERT INTO teams(team_id, owner_account, generation, display_name, approve_new_machines)
SELECT 'perf-team-' || lpad(t::text, 3, '0'), 'user_perf_' || lpad(t::text, 4, '0'), 1, 'Perf Team ' || t, false
FROM generate_series(1, 100) t;

INSERT INTO team_members(team_id, account_id, membership_generation, role, active)
SELECT 'perf-team-001', 'user_perf_' || lpad(i::text, 4, '0'), 1, CASE WHEN i <= 5 THEN 'admin' ELSE 'member' END, true
FROM generate_series(1, 200) i;
INSERT INTO team_members(team_id, account_id, membership_generation, role, active)
SELECT 'perf-team-' || lpad(t::text, 3, '0'),
       'user_perf_' || lpad((((t * 37 + k * 11) % 1000) + 201)::text, 4, '0'), 1, 'member', true
FROM generate_series(2, 100) t, generate_series(1, 2 + t % 20) k
ON CONFLICT DO NOTHING;
INSERT INTO team_members(team_id, account_id, membership_generation, role, active)
SELECT 'perf-team-' || lpad(t::text, 3, '0'), 'user_perf_' || lpad(t::text, 4, '0'), 1, 'member', true
FROM generate_series(2, 100) t
ON CONFLICT DO NOTHING;

-- Every team is on a paid plan (team machine access requires it).
INSERT INTO billing_workspaces(id, team_id, product_code, state, quantity, period_start, period_end)
SELECT 'bw_perf_team_' || t, 'perf-team-' || lpad(t::text, 3, '0'), 'team', 'active', 200, now() - interval '10 days',
       now() + interval '20 days'
FROM generate_series(1, 100) t;

-- 5000 machines: 1000 in team 1 (owned by its members), 100 personal for user 2, 3900 personal spread over users.
CREATE TEMP TABLE perf_machine_plan ON COMMIT DROP AS
SELECT i AS mi,
       CASE WHEN i <= 1000 THEN 'user_perf_' || lpad((1 + i % 200)::text, 4, '0')
            WHEN i <= 1100 THEN 'user_perf_0002'
            ELSE 'user_perf_' || lpad((3 + i % 1198)::text, 4, '0') END AS owner,
       CASE WHEN i <= 1000 THEN 'perf-team-001' END AS team
FROM generate_series(1, 5000) i;

INSERT INTO user_machines(id, user_id, environment_id, platform, architecture, workspace_root, state, seat_state, online,
  enrolled_at, last_seen_at, alias, owner_team_id, installation_generation, worker_generation, connector_state,
  configured_capabilities, observed_capabilities, created_at, updated_at, host_service_version)
SELECT pg_temp.pid('machine', mi), owner, pg_temp.pid('env', mi),
       CASE mi % 3 WHEN 0 THEN 'darwin' ELSE 'linux' END, CASE mi % 2 WHEN 0 THEN 'arm64' ELSE 'amd64' END, '/home/perf',
       CASE WHEN mi % 10 = 0 THEN 'offline' WHEN mi % 97 = 0 THEN 'revoked' ELSE 'online' END,
       'occupied', mi % 10 <> 0, now() - interval '30 days', now() - ((mi % 60) || ' seconds')::interval,
       'm' || mi, team, 1, 1, 'ready',
       ARRAY['file_receive','preview_launch','terminal_host','session_host','ssh_host'],
       ARRAY['file_receive','preview_launch','terminal_host','session_host','ssh_host'],
       now() - ((mi % 400) || ' days')::interval, now() - ((mi % 60) || ' seconds')::interval, '2026.10.20.1'
FROM perf_machine_plan;
UPDATE user_machines SET revoked_at = now() - interval '1 day' WHERE state = 'revoked' AND id LIKE 'machine_%';

INSERT INTO team_resource_bindings(team_id, resource_kind, resource_id, owner_account, active, generation)
SELECT team, 'machine', pg_temp.pid('machine', mi), owner, true, 1 FROM perf_machine_plan WHERE team IS NOT NULL;
INSERT INTO team_machine_grants(team_id, machine_id, audience, capabilities)
SELECT team, pg_temp.pid('machine', mi), 'all_members', ARRAY['terminal','exec','files']
FROM perf_machine_plan WHERE team IS NOT NULL AND mi % 2 = 0;
INSERT INTO team_machine_grants(team_id, machine_id, audience, account_id, capabilities)
SELECT team, pg_temp.pid('machine', mi), 'selected_member', 'user_perf_' || lpad((1 + (mi * 7 + k) % 200)::text, 4, '0'),
       ARRAY['terminal','exec','managed_ssh','files','private_access']
FROM perf_machine_plan, generate_series(1, 3) k WHERE team IS NOT NULL AND mi % 2 = 1
ON CONFLICT DO NOTHING;
INSERT INTO team_machine_grants(team_id, machine_id, audience, account_id, capabilities)
SELECT 'perf-team-001', pg_temp.pid('machine', i), 'selected_member', 'user_perf_0003', ARRAY['terminal','exec']
FROM generate_series(1, 1000, 2) i
ON CONFLICT DO NOTHING;

-- Browser sessions: 50k, mostly expired or revoked; one live session per perf actor is minted by the check.
INSERT INTO sessions(id, user_id, session_hash, csrf_hash, expires_at, revoked_at, created_at, updated_at)
SELECT pg_temp.pid('session', i), 'user_perf_' || lpad((1 + i % 1200)::text, 4, '0'), md5('sh' || i) || md5('sh2' || i),
       md5('cs' || i), now() - ((i % 300) || ' days')::interval + interval '14 days',
       CASE WHEN i % 4 = 0 THEN now() - ((i % 300) || ' days')::interval END,
       now() - ((i % 300) || ' days')::interval, now() - ((i % 300) || ' days')::interval
FROM generate_series(1, 50000) i;

-- CLI client sessions (20k) with access and refresh tokens, and one machine-bound session per machine.
INSERT INTO cli_client_sessions(id, user_id, client_id, client_label, device_type, os, scopes, state, created_at, approved_at,
  last_used_at, revoked_at, revocation_reason, user_machine_id)
SELECT pg_temp.pid('clisession', i), 'user_perf_' || lpad((1 + i % 1200)::text, 4, '0'), 'pb-cli', 'perf laptop ' || i,
       'laptop', 'linux', ARRAY['machines:read','machines:write'], CASE WHEN i % 3 = 0 THEN 'revoked' ELSE 'active' END,
       now() - ((i % 200) || ' days')::interval, now() - ((i % 200) || ' days')::interval, now() - ((i % 50) || ' hours')::interval,
       CASE WHEN i % 3 = 0 THEN now() - ((i % 100) || ' days')::interval END, CASE WHEN i % 3 = 0 THEN 'logout' END, NULL
FROM generate_series(1, 20000) i;
INSERT INTO cli_client_sessions(id, user_id, client_id, client_label, device_type, os, scopes, state, created_at, approved_at,
  last_used_at, user_machine_id)
SELECT pg_temp.pid('machinesession', mi), owner, 'pb-daemon', 'machine m' || mi, 'server', 'linux', ARRAY['machine'], 'active',
       now() - interval '30 days', now() - interval '30 days', now(), pg_temp.pid('machine', mi)
FROM perf_machine_plan;
INSERT INTO cli_access_tokens(token_hash, cli_client_session_id, expires_at, created_at, revoked_at)
SELECT md5('at' || i || '-' || k) || md5('at2' || i || '-' || k), pg_temp.pid('clisession', i),
       now() - ((i % 200) || ' days')::interval + (k || ' hours')::interval, now() - ((i % 200) || ' days')::interval, NULL
FROM generate_series(1, 20000) i, generate_series(1, 5) k;
INSERT INTO cli_refresh_tokens(token_hash, cli_client_session_id, state, expires_at, created_at, rotated_at)
SELECT md5('rt' || i || '-' || k) || md5('rt2' || i || '-' || k), pg_temp.pid('clisession', i),
       CASE WHEN k = 3 AND i % 3 <> 0 THEN 'active' ELSE 'rotated' END, now() + interval '30 days',
       now() - ((i % 200) || ' days')::interval, CASE WHEN k = 3 AND i % 3 <> 0 THEN NULL ELSE now() END
FROM generate_series(1, 20000) i, generate_series(1, 3) k;

-- Audit events: 200k across accounts, team 1 has a long activity history.
INSERT INTO audit_events(id, actor_user_id, actor_type, event_type, resource_type, resource_id, metadata, created_at,
  account_id, actor_id, change_type, outcome, cursor_sequence, activity_workspace_id, activity_machine_id,
  activity_machine_name)
SELECT pg_temp.pid('event', i), 'user_perf_' || lpad((1 + i % 1200)::text, 4, '0'), 'user',
       (ARRAY['machine.terminal.open','machine.exec','machine.file.send','preview.create','tunnel.create','auth.login'])[1 + i % 6],
       CASE WHEN i % 5 = 0 THEN 'team' ELSE 'machine' END,
       CASE WHEN i % 5 = 0 THEN 'perf-team-' || lpad((1 + (i / 5) % 3)::text, 3, '0') ELSE pg_temp.pid('machine', 1 + i % 5000) END,
       jsonb_build_object('n', i), now() - ((200000 - i) || ' minutes')::interval,
       'user_perf_' || lpad((1 + i % 1200)::text, 4, '0'), 'user_perf_' || lpad((1 + i % 1200)::text, 4, '0'),
       'changed', 'changed', i,
       CASE WHEN i % 5 = 0 THEN 'perf-team-' || lpad((1 + (i / 5) % 3)::text, 3, '0') WHEN i % 2 = 0 THEN 'perf-team-001' END,
       pg_temp.pid('machine', 1 + i % 5000), 'm' || (1 + i % 5000)
FROM generate_series(1, 200000) i;

-- Terminal sessions: 50k (most closed), 10 open per machine for the first 1100 machines.
INSERT INTO user_machine_terminal_sessions(id, user_machine_id, name, launch_cwd, desired_state, runtime_state,
  last_activity_at, owner_account, owner_team_id, created_at, updated_at, closed_at, deleted_at)
SELECT pg_temp.pid('terminal', i), pg_temp.pid('machine', 1 + i % 5000), 's' || i, '/home/perf',
       CASE WHEN i <= 11000 THEN 'open' ELSE 'closed' END, CASE WHEN i <= 11000 THEN 'running' ELSE 'exited' END,
       now() - ((i % 1000) || ' minutes')::interval, p.owner, p.team,
       now() - ((i % 90) || ' days')::interval, now(), CASE WHEN i > 11000 THEN now() - ((i % 90) || ' days')::interval END,
       CASE WHEN i > 30000 THEN now() - ((i % 90) || ' days')::interval END
FROM generate_series(1, 50000) i JOIN perf_machine_plan p ON p.mi = 1 + (i % 5000);

-- Machine access sessions: 50k (mostly revoked/expired).
INSERT INTO user_machine_access_sessions(id, user_machine_id, user_id, environment_id, cli_client_session_id, http_base_url,
  helper_terminal_session_id, state, revoked_at, expires_at, created_at, updated_at, team_id, capabilities, credential_kind,
  revocation_reason, helper_revoked_at)
SELECT pg_temp.pid('access', i), pg_temp.pid('machine', 1 + i % 5000), p.owner, pg_temp.pid('env', 1 + i % 5000),
       pg_temp.pid('clisession', 1 + i % 20000), 'https://helper.example.test', 'helper_' || i,
       CASE WHEN i % 20 = 0 THEN 'active' ELSE 'revoked' END,
       CASE WHEN i % 20 = 0 THEN NULL ELSE now() - ((i % 60) || ' days')::interval END,
       now() - ((i % 60) || ' days')::interval + interval '1 hour', now() - ((i % 60) || ' days')::interval, now(),
       p.team, ARRAY['terminal'], 'control_plane_signed',
       CASE WHEN i % 20 = 0 THEN NULL ELSE 'expired' END, CASE WHEN i % 20 = 0 THEN NULL ELSE now() END
FROM generate_series(1, 50000) i JOIN perf_machine_plan p ON p.mi = 1 + (i % 5000);

-- Previews: 20k leases (95% stopped), tunnels: 5k with 2 routes each.
INSERT INTO preview_leases(id, endpoint, account_id, actor_id, owner_machine_id, owner_session_id, target_scheme,
  target_address, lease_deadline, allocation_state, edge_state, origin_state, terminal_state, created_at, stopped_at,
  owner_session_kind, owner_team_id, last_renewed_at, owner_last_seen_at)
SELECT pg_temp.pid('preview', i), 'https://p' || i || '.runtime.example.test', p.owner, p.owner, pg_temp.pid('machine', p.mi),
       pg_temp.pid('clisession', 1 + i % 20000), 'http', '127.0.0.1:3000',
       now() - ((i % 90) || ' days')::interval + interval '8 hours',
       CASE WHEN i % 20 = 0 THEN 'ready' ELSE 'released' END, CASE WHEN i % 20 = 0 THEN 'ready' ELSE 'released' END, 'ready',
       CASE WHEN i % 20 = 0 THEN 'active' ELSE 'stopped' END, now() - ((i % 90) || ' days')::interval,
       CASE WHEN i % 20 = 0 THEN NULL ELSE now() - ((i % 90) || ' days')::interval + interval '1 hour' END,
       'local_lease', p.team, now(), now()
FROM generate_series(1, 20000) i JOIN perf_machine_plan p ON p.mi = 1 + (i * 13) % 1500;
UPDATE preview_leases SET lease_deadline = now() + interval '8 hours' WHERE terminal_state = 'active' AND id LIKE 'preview_%';

INSERT INTO tunnels(id, account_id, name, desired_state, access_mode, stable_endpoint_id, stable_endpoint, created_by_host_id,
  created_by_actor_id, summary_code, created_at, updated_at, deleted_at, owner_team_id)
SELECT pg_temp.pid('tunnel', i), p.owner, 't' || i, CASE WHEN i % 4 = 0 THEN 'deleted' ELSE 'active' END, 'public',
       'endpoint_' || pg_temp.uuid4('ep' || i), 'https://t' || i || '.example.test', pg_temp.pid('machine', p.mi), p.owner,
       'ready', now() - ((i % 200) || ' days')::interval, now() - ((i % 200) || ' hours')::interval,
       CASE WHEN i % 4 = 0 THEN now() - ((i % 100) || ' days')::interval END, p.team
FROM generate_series(1, 5000) i JOIN perf_machine_plan p ON p.mi = 1 + (i * 7) % 1500;
INSERT INTO tunnel_routes(id, tunnel_id, name, match_type, match_hostname, origin_scheme, origin_address,
  created_by_actor_id, updated_by_actor_id)
SELECT pg_temp.pid('route', i * 2 + k), t.id, 'r' || k, 'exact', 'r' || k || '-t' || i || '.example.test', 'http',
       '127.0.0.1:' || (8000 + k), t.account_id, t.account_id
FROM generate_series(1, 5000) i JOIN tunnels t ON t.id = pg_temp.pid('tunnel', i), generate_series(0, 1) k;

-- Per-member resource grants for team 1 previews/tunnels, team inbox history, bandwidth events.
INSERT INTO team_resource_bindings(team_id, resource_kind, resource_id, owner_account, active, generation)
SELECT owner_team_id, 'preview', id, account_id, true, 1 FROM preview_leases WHERE owner_team_id IS NOT NULL;
INSERT INTO team_resource_bindings(team_id, resource_kind, resource_id, owner_account, active, generation)
SELECT owner_team_id, 'tunnel', id, account_id, true, 1 FROM tunnels WHERE owner_team_id IS NOT NULL;
INSERT INTO team_resource_grants(team_id, account_id, resource_kind, resource_id, permission, generation, active)
SELECT b.team_id, 'user_perf_' || lpad((1 + (abs(hashtext(b.resource_id)) + k) % 200)::text, 4, '0'), b.resource_kind,
       b.resource_id, 'use', 1, true
FROM team_resource_bindings b, generate_series(1, 2) k WHERE b.resource_kind IN ('preview', 'tunnel')
ON CONFLICT DO NOTHING;

INSERT INTO team_inbox_requests(request_id, operation_id, team_id, sender_account, recipient_account, source_machine_id,
  destination_machine_id, batch_id, manifest_digest, manifest, status, decision_kind, policy_generation, expires_at,
  created_at, updated_at)
SELECT pg_temp.pid('request', i), pg_temp.pid('operation', i), 'perf-team-001',
       'user_perf_' || lpad((1 + i % 200)::text, 4, '0'), 'user_perf_' || lpad((1 + (i + 1) % 200)::text, 4, '0'),
       pg_temp.pid('machine', 1 + i % 1000), pg_temp.pid('machine', 1 + (i + 3) % 1000), 'batch_' || i, md5('d' || i) || md5('e' || i),
       '[{"name":"report.pdf","size":1024}]', CASE WHEN i % 10 = 0 THEN 'pending' ELSE 'completed' END, 'manual', 1,
       now() + interval '1 day', now() - ((i % 60) || ' days')::interval, now()
FROM generate_series(1, 5000) i;

INSERT INTO resource_bandwidth_events(operation_id, workspace_id, resource_kind, resource_id, resource_name, machine_id,
  machine_name, mode, hosting, upload_bytes, download_bytes, interval_start, interval_end, actor_account_id)
SELECT pg_temp.pid('bw', i), CASE WHEN i % 2 = 0 THEN 'perf-team-001' ELSE 'user_perf_' || lpad((1 + i % 1200)::text, 4, '0') END,
       'machine', pg_temp.pid('machine', 1 + i % 5000), 'm' || (1 + i % 5000), pg_temp.pid('machine', 1 + i % 5000),
       'm' || (1 + i % 5000), 'relay', 'managed', i % 100000, i % 300000,
       now() - ((200000 - i) || ' minutes')::interval, now() - ((200000 - i) || ' minutes')::interval + interval '1 minute',
       'user_perf_' || lpad((1 + i % 200)::text, 4, '0')
FROM generate_series(1, 200000) i;

-- E2EE: one root per account, keys for every CLI session.
INSERT INTO account_e2ee_roots(user_id, public_key, fingerprint)
SELECT id, decode(md5('rk' || id) || md5('rk2' || id), 'hex'), decode(md5('rf' || id) || md5('rf2' || id), 'hex') FROM users
WHERE id LIKE 'user_perf_%';
INSERT INTO account_e2ee_keys(key_id, user_id, public_key, fingerprint, cli_client_session_id, created_at, revoked_at,
  revocation_reason)
SELECT 'aek_' || md5('k' || c.id), c.user_id, decode(md5('kp' || c.id) || md5('kp2' || c.id), 'hex'),
       decode(md5('kf' || c.id) || md5('kf2' || c.id), 'hex'), c.id, c.created_at, c.revoked_at,
       CASE WHEN c.revoked_at IS NOT NULL THEN 'account_revoked' END
FROM cli_client_sessions c WHERE c.id LIKE 'clisession_%';

ANALYZE;
