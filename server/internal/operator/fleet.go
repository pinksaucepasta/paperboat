package operator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/releases"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
)

// The operator Fleet page: which pb version every machine runs, how far each release rollout got and which machines
// are offline. Everything but the rollout chart is read live from user_machines (reporter_version, written by every
// heartbeat), its update observation and release_authority_bundles; the chart reads fleet_version_daily, one row per
// day, platform and version (SnapshotFleet). Prior art: teleport lib/web/managed_updates.go (agent version counts per
// rollout) and tailscale tailcfg Hostinfo.IPNVersion (the client reports its own version).

// fleetMachines is the machine set the page counts: enrolled, not removed. version is what the machine reports; a
// machine that never reported one falls back to its update agent's, then to "unknown". $1 is the offline cutoff in
// microseconds.
const (
	fleetMachines = ` FROM user_machines m LEFT JOIN user_machine_update_observations o ON o.user_machine_id=m.id
 WHERE m.deleted_at IS NULL AND m.revoked_at IS NULL AND m.state IN ('online','offline','disconnected')`
	fleetVersion = `left(coalesce(nullif(m.reporter_version,''),o.current_version,'unknown'),64)`
	// fleetOfficial: the machine's executable is a published, signed release target of its version, platform and arch.
	fleetOfficial = `EXISTS(SELECT 1 FROM release_target_hashes t WHERE t.platform=m.platform AND t.architecture=m.architecture
 AND t.version=` + fleetVersion + ` AND t.sha256=m.reporter_sha256)`
	fleetOnline = `(m.online AND m.state='online' AND m.last_seen_at>now()-($1*interval '1 microsecond'))`
)

// Offline buckets by how long ago the machine was last seen.
var fleetBuckets = []struct{ name, where string }{
	{"under_1_day", `m.last_seen_at>now()-interval '1 day'`},
	{"1_to_7_days", `m.last_seen_at<=now()-interval '1 day' AND m.last_seen_at>now()-interval '7 days'`},
	{"7_to_30_days", `m.last_seen_at<=now()-interval '7 days' AND m.last_seen_at>now()-interval '30 days'`},
	{"over_30_days", `m.last_seen_at<=now()-interval '30 days'`},
	{"never_seen", `m.last_seen_at IS NULL`},
}

// versionKey orders release versions (YYYY.M.D.build); a dev build or "unknown" has no order.
func versionKey(v string) ([4]int, bool) {
	var key [4]int
	parts := strings.Split(v, ".")
	if len(parts) != 4 {
		return key, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return key, false
		}
		key[i] = n
	}
	return key, true
}

// versionLess reports a < b; both must be ordered versions.
func versionLess(a, b [4]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

type FleetVersionRow struct {
	Platform string `json:"platform"`
	Version  string `json:"version"`
	Official bool   `json:"official"` // the machines' executable matches a signed release target of this version
	Machines int64  `json:"machines"`
	Online   int64  `json:"online"`
	Users    int64  `json:"users"`
	Teams    int64  `json:"teams"`
}

type FleetPlatform struct {
	Platform     string `json:"platform"`
	Machines     int64  `json:"machines"`
	Online       int64  `json:"online"`
	Users        int64  `json:"users"`
	Teams        int64  `json:"teams"`
	OldestOnline string `json:"oldest_online_version,omitempty"`
}

// FleetFailure is what machines whose last update attempt failed or rolled back report (the server-recorded reason).
type FleetFailure struct {
	TargetVersion string `json:"target_version"`
	State         string `json:"state"`
	ErrorCode     string `json:"error_code"`
	Machines      int64  `json:"machines"`
}

// FleetBuilds counts the machines running a build that is not official: its executable hash is not that of a signed
// release target (a dev or custom build, whatever version string it reports). Machines that reported no version at
// all ("unknown") are not counted.
type FleetBuilds struct {
	Machines int64 `json:"machines"`
	Online   int64 `json:"online"`
	Versions int64 `json:"versions"`
}

// FleetSelfHostedKind counts customer-installed relays or edges: registered, online now (the installation is ready)
// and reporting (its node sent a capacity report, which carries its traffic, within the node stale window).
type FleetSelfHostedKind struct {
	Registered int64 `json:"registered"`
	Online     int64 `json:"online"`
	Reporting  int64 `json:"reporting"`
}

// FleetSelfHosted counts nodes owned by teams and accounts; the operator's own (global) installations belong on
// Infrastructure. Counts only: no node names, hosts or addresses.
type FleetSelfHosted struct {
	Relays   FleetSelfHostedKind `json:"relays"`
	Edges    FleetSelfHostedKind `json:"edges"`
	Teams    int64               `json:"teams"`
	Accounts int64               `json:"accounts"`
}

type FleetVersions struct {
	ObservedAt   time.Time         `json:"observed_at"`
	Machines     int64             `json:"machines"`
	Online       int64             `json:"online"`
	Users        int64             `json:"users"`
	Teams        int64             `json:"teams"`
	OldestOnline string            `json:"oldest_online_version,omitempty"`
	Platforms    []FleetPlatform   `json:"platforms"`
	Versions     []FleetVersionRow `json:"versions"`
	Failures     []FleetFailure    `json:"failures"`
	NonOfficial  FleetBuilds       `json:"non_official"`
	SelfHosted   FleetSelfHosted   `json:"self_hosted"`
}

func (s *Service) readFleetVersions(ctx context.Context, tx *db.Tx) (FleetVersions, error) {
	out := FleetVersions{ObservedAt: time.Now().UTC(), Platforms: []FleetPlatform{}, Versions: []FleetVersionRow{}, Failures: []FleetFailure{}}
	stale := s.hostStaleAfter.Microseconds()
	if err := tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE `+fleetOnline+`),count(DISTINCT m.user_id),count(DISTINCT m.owner_team_id)`+fleetMachines, stale).
		Scan(&out.Machines, &out.Online, &out.Users, &out.Teams); err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT m.platform,`+fleetVersion+`,`+fleetOfficial+`,count(*),count(*) FILTER(WHERE `+fleetOnline+`),count(DISTINCT m.user_id),count(DISTINCT m.owner_team_id)`+fleetMachines+` GROUP BY 1,2,3`, stale)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var v FleetVersionRow
		if err := rows.Scan(&v.Platform, &v.Version, &v.Official, &v.Machines, &v.Online, &v.Users, &v.Teams); err != nil {
			return out, closeRowsError(rows, err)
		}
		out.Versions = append(out.Versions, v)
	}
	if err := closeRowsError(rows, rows.Err()); err != nil {
		return out, err
	}
	sort.Slice(out.Versions, func(i, j int) bool {
		a, b := out.Versions[i], out.Versions[j]
		ka, oka := versionKey(a.Version)
		kb, okb := versionKey(b.Version)
		switch {
		case a.Platform != b.Platform:
			return a.Platform < b.Platform
		case oka && okb && ka != kb:
			return versionLess(kb, ka) // newest first
		case oka != okb:
			return oka
		case a.Version != b.Version:
			return a.Version > b.Version
		}
		return a.Official && !b.Official
	})
	// Per-platform totals need distinct users, which cannot be summed from version rows.
	prows, err := tx.Query(ctx, `SELECT m.platform,count(*),count(*) FILTER(WHERE `+fleetOnline+`),count(DISTINCT m.user_id),count(DISTINCT m.owner_team_id)`+fleetMachines+` GROUP BY 1 ORDER BY 1`, stale)
	if err != nil {
		return out, err
	}
	for prows.Next() {
		var p FleetPlatform
		if err := prows.Scan(&p.Platform, &p.Machines, &p.Online, &p.Users, &p.Teams); err != nil {
			return out, closeRowsError(prows, err)
		}
		out.Platforms = append(out.Platforms, p)
	}
	if err := closeRowsError(prows, prows.Err()); err != nil {
		return out, err
	}
	out.OldestOnline = oldestOnline(out.Versions, "")
	for i := range out.Platforms {
		out.Platforms[i].OldestOnline = oldestOnline(out.Versions, out.Platforms[i].Platform)
	}
	versions := map[string]bool{}
	for _, r := range out.Versions {
		if !r.Official && r.Version != "unknown" {
			out.NonOfficial.Machines += r.Machines
			out.NonOfficial.Online += r.Online
			versions[r.Version] = true
		}
	}
	out.NonOfficial.Versions = int64(len(versions))
	if out.Failures, err = fleetFailures(ctx, tx); err != nil {
		return out, err
	}
	out.SelfHosted, err = s.fleetSelfHosted(ctx, tx)
	return out, err
}

// fleetSelfHosted counts the active customer installations (scope account or team, as the traffic page does); a
// capability of relay is a relay, anything else an edge.
func (s *Service) fleetSelfHosted(ctx context.Context, tx *db.Tx) (FleetSelfHosted, error) {
	var out FleetSelfHosted
	rows, err := tx.Query(ctx, `SELECT i.capability='relay',count(*),count(*) FILTER(WHERE `+selfhost.InstallationReadySQL+`),
 count(*) FILTER(WHERE EXISTS(SELECT 1 FROM node_capacity c WHERE c.node_id=i.node_id AND c.reported_at>now()-($1*interval '1 microsecond')))
 FROM selfhost_installations i JOIN control_tunnel_nodes n ON n.id=i.node_id
 WHERE i.state='active' AND i.scope_kind IN ('account','team') GROUP BY 1`, s.nodeStaleAfter.Microseconds())
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var relay bool
		var k FleetSelfHostedKind
		if err := rows.Scan(&relay, &k.Registered, &k.Online, &k.Reporting); err != nil {
			return out, closeRowsError(rows, err)
		}
		if relay {
			out.Relays = k
		} else {
			out.Edges = k
		}
	}
	if err := closeRowsError(rows, rows.Err()); err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `SELECT count(DISTINCT scope_id) FILTER(WHERE scope_kind='team'),count(DISTINCT scope_id) FILTER(WHERE scope_kind='account')
 FROM selfhost_installations WHERE state='active' AND scope_kind IN ('account','team')`).Scan(&out.Teams, &out.Accounts)
	return out, err
}

// oldestOnline is the oldest official, ordered version some machine of the platform (all, when empty) runs while online.
func oldestOnline(rows []FleetVersionRow, platform string) string {
	oldest, best := "", [4]int{}
	for _, r := range rows {
		key, ok := versionKey(r.Version)
		if r.Online == 0 || !ok || !r.Official || (platform != "" && r.Platform != platform) {
			continue
		}
		if oldest == "" || versionLess(key, best) {
			oldest, best = r.Version, key
		}
	}
	return oldest
}

func fleetFailures(ctx context.Context, tx *db.Tx) ([]FleetFailure, error) {
	rows, err := tx.Query(ctx, `SELECT coalesce(o.target_version,''),o.state,coalesce(o.error_code,''),count(*)`+fleetMachines+`
 AND o.state IN ('failed','rolled_back') GROUP BY 1,2,3 ORDER BY 4 DESC,1 DESC,2,3 LIMIT 50`)
	if err != nil {
		return nil, err
	}
	out := []FleetFailure{}
	for rows.Next() {
		var f FleetFailure
		if err := rows.Scan(&f.TargetVersion, &f.State, &f.ErrorCode, &f.Machines); err != nil {
			return nil, closeRowsError(rows, err)
		}
		out = append(out, f)
	}
	return out, closeRowsError(rows, rows.Err())
}

// FleetPoint is one day of a rollout chart: machines with an ordered version that day, and those on the release or
// newer; failed counts machines whose update to the release failed or rolled back.
type FleetPoint struct {
	Day     string `json:"day"`
	Total   int64  `json:"total"`
	Updated int64  `json:"updated"`
	Failed  int64  `json:"failed"`
}

type FleetRollout struct {
	Version           string         `json:"version"`
	Platforms         []string       `json:"platforms"`
	StartedAt         time.Time      `json:"started_at"`
	Action            string         `json:"action"` // the latest signed decision: promote, pause, quarantine or revoke
	RolloutPercentage int            `json:"rollout_percentage"`
	Updated           int64          `json:"updated"`
	Older             int64          `json:"older"`
	Other             int64          `json:"other"` // dev builds and unknown versions: no order to compare
	Failed            int64          `json:"failed"`
	RolledBack        int64          `json:"rolled_back"`
	UnreachableSince  int64          `json:"offline_since_start"` // older, last seen before the rollout began (or never): could not have updated
	OfflineNow        int64          `json:"offline_now"`         // older, seen since the start, offline now
	Failures          []FleetFailure `json:"failures"`
	Series            []FleetPoint   `json:"series"`
}

type FleetRollouts struct {
	ObservedAt time.Time      `json:"observed_at"`
	Rollouts   []FleetRollout `json:"rollouts"`
}

const fleetRolloutLimit = 8

// readFleetRollouts reads the signed release decisions (newest first), then counts the live fleet against each.
func (s *Service) readFleetRollouts(ctx context.Context, tx *db.Tx) (FleetRollouts, error) {
	out := FleetRollouts{ObservedAt: time.Now().UTC(), Rollouts: []FleetRollout{}}
	rows, err := tx.Query(ctx, `SELECT version,platform,action,issued_at,coalesce((payload->>'rollout_percentage')::int,0)
 FROM release_authority_bundles WHERE issued_at>now()-interval '400 days' ORDER BY issued_at,policy_revision LIMIT 5000`)
	if err != nil {
		return out, err
	}
	byVersion := map[string]*FleetRollout{}
	for rows.Next() {
		var version, platform, action string
		var issued time.Time
		var pct int
		if err := rows.Scan(&version, &platform, &action, &issued, &pct); err != nil {
			return out, closeRowsError(rows, err)
		}
		r := byVersion[version]
		if r == nil {
			r = &FleetRollout{Version: version, Platforms: []string{}, Failures: []FleetFailure{}, Series: []FleetPoint{}}
			byVersion[version] = r
		}
		if action == "promote" && (r.StartedAt.IsZero() || issued.Before(r.StartedAt)) {
			r.StartedAt = issued.UTC()
		}
		if action == "promote" && !slices.Contains(r.Platforms, platform) {
			r.Platforms = append(r.Platforms, platform)
		}
		r.Action = action // ordered by time: the last one is the current decision
		if action == "promote" {
			r.RolloutPercentage = pct
		}
	}
	if err := closeRowsError(rows, rows.Err()); err != nil {
		return out, err
	}
	for _, r := range byVersion {
		if !r.StartedAt.IsZero() {
			out.Rollouts = append(out.Rollouts, *r)
		}
	}
	sort.Slice(out.Rollouts, func(i, j int) bool { return out.Rollouts[i].StartedAt.After(out.Rollouts[j].StartedAt) })
	if len(out.Rollouts) > fleetRolloutLimit {
		out.Rollouts = out.Rollouts[:fleetRolloutLimit]
	}
	for i := range out.Rollouts {
		sort.Strings(out.Rollouts[i].Platforms)
		if err := s.fillRollout(ctx, tx, &out.Rollouts[i]); err != nil {
			return out, err
		}
	}
	return out, nil
}

// fillRollout counts the machines of the release's platforms against its version.
func (s *Service) fillRollout(ctx context.Context, tx *db.Tx, r *FleetRollout) error {
	target, ordered := versionKey(r.Version)
	if !ordered {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT `+fleetVersion+`,count(*),count(*) FILTER(WHERE m.last_seen_at IS NULL OR m.last_seen_at<$3),
 count(*) FILTER(WHERE NOT `+fleetOnline+` AND m.last_seen_at>=$3)`+fleetMachines+` AND m.platform=ANY($2) GROUP BY 1`, s.hostStaleAfter.Microseconds(), r.Platforms, r.StartedAt)
	if err != nil {
		return err
	}
	for rows.Next() {
		var version string
		var n, beforeStart, offlineNow int64
		if err := rows.Scan(&version, &n, &beforeStart, &offlineNow); err != nil {
			return closeRowsError(rows, err)
		}
		key, ok := versionKey(version)
		switch {
		case !ok:
			r.Other += n
		case versionLess(key, target):
			r.Older += n
			r.UnreachableSince += beforeStart
			r.OfflineNow += offlineNow
		default:
			r.Updated += n
		}
	}
	if err := closeRowsError(rows, rows.Err()); err != nil {
		return err
	}
	if err := s.fillRolloutFailures(ctx, tx, r, target); err != nil {
		return err
	}
	return s.fillRolloutSeries(ctx, tx, r, target)
}

// fillRolloutFailures counts machines that tried this release and ended failed or rolled back while still older.
func (s *Service) fillRolloutFailures(ctx context.Context, tx *db.Tx, r *FleetRollout, target [4]int) error {
	rows, err := tx.Query(ctx, `SELECT `+fleetVersion+`,o.state,coalesce(o.error_code,''),count(*)`+fleetMachines+`
 AND o.target_version=$1 AND o.state IN ('failed','rolled_back') AND m.platform=ANY($2) GROUP BY 1,2,3`, r.Version, r.Platforms)
	if err != nil {
		return err
	}
	reasons := map[[2]string]int64{}
	for rows.Next() {
		var version, state, code string
		var n int64
		if err := rows.Scan(&version, &state, &code, &n); err != nil {
			return closeRowsError(rows, err)
		}
		if key, ok := versionKey(version); !ok || !versionLess(key, target) {
			continue
		}
		reasons[[2]string{state, code}] += n
		if state == "failed" {
			r.Failed += n
		} else {
			r.RolledBack += n
		}
	}
	if err := closeRowsError(rows, rows.Err()); err != nil {
		return err
	}
	for k, n := range reasons {
		r.Failures = append(r.Failures, FleetFailure{TargetVersion: r.Version, State: k[0], ErrorCode: k[1], Machines: n})
	}
	sort.Slice(r.Failures, func(i, j int) bool {
		a, b := r.Failures[i], r.Failures[j]
		return a.Machines > b.Machines || a.Machines == b.Machines && a.State+a.ErrorCode < b.State+b.ErrorCode
	})
	return nil
}

// fillRolloutSeries builds the chart from the daily rows since the rollout began.
func (s *Service) fillRolloutSeries(ctx context.Context, tx *db.Tx, r *FleetRollout, target [4]int) error {
	rows, err := tx.Query(ctx, `SELECT day::text,version,sum(machines),sum(failed+rolled_back) FROM fleet_version_daily
 WHERE day>=($1::timestamptz AT TIME ZONE 'UTC')::date AND platform=ANY($2) GROUP BY 1,2 ORDER BY 1`, r.StartedAt, r.Platforms)
	if err != nil {
		return err
	}
	var point *FleetPoint
	for rows.Next() {
		var day, version string
		var machines, failed int64
		if err := rows.Scan(&day, &version, &machines, &failed); err != nil {
			return closeRowsError(rows, err)
		}
		if point == nil || point.Day != day {
			r.Series = append(r.Series, FleetPoint{Day: day})
			point = &r.Series[len(r.Series)-1]
		}
		if version == r.Version {
			point.Failed += failed
		}
		if key, ok := versionKey(version); ok {
			point.Total += machines
			if !versionLess(key, target) {
				point.Updated += machines
			}
		}
	}
	return closeRowsError(rows, rows.Err())
}

type FleetBucket struct {
	Bucket   string `json:"bucket"`
	Machines int64  `json:"machines"`
}

type FleetOfflineMachine struct {
	ID           string     `json:"id"`
	Alias        string     `json:"alias"`
	Platform     string     `json:"platform"`
	Architecture string     `json:"architecture"`
	Version      string     `json:"version"`
	State        string     `json:"state"`
	AccountID    string     `json:"account_id"`
	TeamID       string     `json:"team_id,omitempty"`
	LastSeenAt   *time.Time `json:"last_seen_at,omitempty"`
	Bucket       string     `json:"bucket"`
}

type FleetOffline struct {
	Counted
	Machines  int64                 `json:"fleet_machines"`
	Offline   int64                 `json:"offline"`
	Buckets   []FleetBucket         `json:"buckets"`
	Items     []FleetOfflineMachine `json:"items"`
	NextAfter string                `json:"next_after,omitempty"`
}

// readFleetOffline lists the offline machines (filter: bucket, platform, version, q), longest unseen first, paged by
// offset. The buckets count the whole fleet, whatever the filter.
func (s *Service) readFleetOffline(ctx context.Context, tx *db.Tx, target, after string, limit int) (FleetOffline, error) {
	out := FleetOffline{Buckets: []FleetBucket{}, Items: []FleetOfflineMachine{}}
	f, err := parseFilter(target, "bucket", "platform", "version", "q")
	offset, convErr := strconv.Atoi(after)
	if after == "" {
		offset, convErr = 0, nil
	}
	known := f["bucket"] == ""
	for _, b := range fleetBuckets {
		known = known || f["bucket"] == b.name
	}
	if err != nil || convErr != nil || offset < 0 || !known || !oneOf(f["platform"], "linux", "darwin", "windows") {
		return out, ErrInvalid
	}
	stale := s.hostStaleAfter.Microseconds()
	bucketCase, filter := `CASE`, ``
	for _, b := range fleetBuckets {
		bucketCase += ` WHEN ` + b.where + ` THEN '` + b.name + `'`
		if f["bucket"] == b.name {
			filter = ` AND (` + b.where + `)`
		}
	}
	bucketCase += ` ELSE 'never_seen' END`
	offline := ` AND NOT ` + fleetOnline
	crows, err := tx.Query(ctx, `SELECT `+bucketCase+`,count(*)`+fleetMachines+offline+` GROUP BY 1`, stale)
	if err != nil {
		return out, err
	}
	seen := map[string]int64{}
	for crows.Next() {
		var name string
		var n int64
		if err := crows.Scan(&name, &n); err != nil {
			return out, closeRowsError(crows, err)
		}
		seen[name] = n
		out.Offline += n
	}
	if err := closeRowsError(crows, crows.Err()); err != nil {
		return out, err
	}
	for _, b := range fleetBuckets {
		out.Buckets = append(out.Buckets, FleetBucket{Bucket: b.name, Machines: seen[b.name]})
	}
	if err := tx.QueryRow(ctx, `SELECT count(*)`+fleetMachines).Scan(&out.Machines); err != nil {
		return out, err
	}
	where := fleetMachines + offline + filter + ` AND ($2='' OR m.platform=$2) AND ($3='' OR ` + fleetVersion + `=$3)
 AND ($4='' OR m.id ILIKE $4 ESCAPE '\' OR m.alias ILIKE $4 ESCAPE '\' OR m.user_id ILIKE $4 ESCAPE '\' OR m.owner_team_id ILIKE $4 ESCAPE '\')`
	args := []any{stale, f["platform"], f["version"], contains(f["q"])}
	if out.Counted, err = countRows(ctx, tx, strings.TrimPrefix(where, " FROM "), args...); err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT m.id,m.alias,m.platform,m.architecture,`+fleetVersion+`,m.state,m.user_id,coalesce(m.owner_team_id,''),m.last_seen_at,`+bucketCase+where+
		` ORDER BY m.last_seen_at ASC NULLS FIRST,m.id LIMIT $5 OFFSET $6`, append(args, limit, offset)...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var m FleetOfflineMachine
		var seenAt sql.NullTime
		if err := rows.Scan(&m.ID, &m.Alias, &m.Platform, &m.Architecture, &m.Version, &m.State, &m.AccountID, &m.TeamID, &seenAt, &m.Bucket); err != nil {
			return out, closeRowsError(rows, err)
		}
		if seenAt.Valid {
			t := seenAt.Time.UTC()
			m.LastSeenAt = &t
		}
		out.Items = append(out.Items, m)
	}
	if err := closeRowsError(rows, rows.Err()); err != nil {
		return out, err
	}
	if next := offset + len(out.Items); len(out.Items) == limit && int64(next) < out.Total {
		out.NextAfter = strconv.Itoa(next)
	}
	return out, nil
}

// SnapshotFleet rewrites today's rows of fleet_version_daily from the live fleet (the last write of a day stays), and
// deletes the days past the retention window.
func (s *Service) SnapshotFleet(ctx context.Context) error {
	err := s.snapshotFleetVersions(ctx)
	if recordErr := s.recordReleaseTargets(ctx); recordErr != nil {
		err = errors.Join(err, recordErr)
	}
	return err
}

// recordReleaseTargets remembers the hash of every signed pb target the origin lists now: targets.json holds only the
// newest target of each platform, and machines keep running older ones.
func (s *Service) recordReleaseTargets(ctx context.Context) error {
	if s.releaseDir == "" {
		return nil
	}
	current, err := releases.ReadCurrent(s.releaseDir)
	if err != nil {
		return fmt.Errorf("record release target hashes: %w", err)
	}
	return s.recordTargets(ctx, current.Assets)
}

func (s *Service) recordTargets(ctx context.Context, assets map[string]releases.Asset) error {
	return s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		for _, a := range assets {
			if _, err := tx.Exec(ctx, `INSERT INTO release_target_hashes(version,platform,architecture,sha256) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
				a.Version, a.Platform, a.Architecture, a.SHA256); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) snapshotFleetVersions(ctx context.Context) error {
	return s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM fleet_version_daily WHERE day>=(now() AT TIME ZONE 'UTC')::date`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `WITH f AS (SELECT m.platform,`+fleetVersion+` AS v,`+fleetOnline+` AS up,o.state,o.target_version`+fleetMachines+`),
 a AS (SELECT platform,v AS version,count(*) AS machines,count(*) FILTER(WHERE up) AS online FROM f GROUP BY 1,2),
 b AS (SELECT platform,left(target_version,64) AS version,count(*) FILTER(WHERE state='failed') AS failed,count(*) FILTER(WHERE state='rolled_back') AS rolled_back
  FROM f WHERE state IN ('failed','rolled_back') AND target_version IS NOT NULL AND target_version<>v GROUP BY 1,2)
 INSERT INTO fleet_version_daily(day,platform,version,machines,online,failed,rolled_back)
 SELECT (now() AT TIME ZONE 'UTC')::date,platform,version,coalesce(a.machines,0),coalesce(a.online,0),coalesce(b.failed,0),coalesce(b.rolled_back,0)
 FROM a FULL JOIN b USING(platform,version)`, s.hostStaleAfter.Microseconds())
		return err
	})
}
