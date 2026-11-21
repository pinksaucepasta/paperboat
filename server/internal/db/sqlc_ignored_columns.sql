-- sqlc only, never applied to a database: columns the code must no longer name, so generated queries
-- (and `*` expansions) leave them out while servers of the previous build still read them. The next
-- deploy's migration drops them for real and empties this file (GitLab ignore-then-drop).
ALTER TABLE tunnel_domains DROP COLUMN ownership_state, DROP COLUMN last_verified_at;
ALTER TABLE preview_domains DROP COLUMN ownership_state, DROP COLUMN last_verified_at;
