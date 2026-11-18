package posthogquery

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Pair is a number for the chosen range and the same number for the range before it.
type Pair struct {
	Value    int64 `json:"value"`
	Previous int64 `json:"previous"`
}

type Day struct {
	Day      string `json:"day"`
	Visitors int64  `json:"visitors"`
}

type Count struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// Source is a referrer or campaign: the visits it brought and how many of those visits clicked a sign-up link.
type Source struct {
	Name         string `json:"name"`
	Visitors     int64  `json:"visitors"`
	SignupClicks int64  `json:"signup_clicks"`
}

type FunnelStep struct {
	Step  string `json:"step"`
	Count int64  `json:"count"`
}

// Retention is week-1 retention: of the accounts that signed up at least a week ago in the range (and the range
// before it), how many were still active a week or more after signing up.
type Retention struct {
	Size             int64 `json:"size"`
	Retained         int64 `json:"retained"`
	PreviousSize     int64 `json:"previous_size"`
	PreviousRetained int64 `json:"previous_retained"`
}

// Site is the marketing site: who came, from where, and how many went on to sign up. An insight that failed is
// named in Errors and left empty. Visitors are browser tabs (a random id per tab), not people.
type Site struct {
	ObservedAt time.Time         `json:"observed_at"`
	Days       int               `json:"days"`
	URL        string            `json:"url,omitempty"`
	Online     int64             `json:"online"`
	Visitors   Pair              `json:"visitors"`
	Signups    Pair              `json:"signups"`
	Daily      []Day             `json:"daily"`
	Referrers  []Source          `json:"referrers"`
	Campaigns  []Source          `json:"campaigns"`
	Pages      []Count           `json:"pages"`
	Countries  []Count           `json:"countries"`
	Errors     map[string]string `json:"errors,omitempty"`
}

// Console is the product: who is active, whether new accounts reach their first use, and which features they use.
type Console struct {
	ObservedAt     time.Time         `json:"observed_at"`
	Days           int               `json:"days"`
	URL            string            `json:"url,omitempty"`
	Online         int64             `json:"online"`
	DailyActive    Pair              `json:"daily_active"`
	WeeklyActive   Pair              `json:"weekly_active"`
	Paid           Pair              `json:"paid"`
	ActiveAccounts int64             `json:"active_accounts"`
	Funnel         []FunnelStep      `json:"funnel"`
	Retention      Retention         `json:"retention"`
	Adoption       []Count           `json:"adoption"`
	Errors         map[string]string `json:"errors,omitempty"`
}

// Features the adoption view counts, by the server's feature_used label.
const adoptionFeatures = `'terminal','exec','transfer','preview','tunnel','env','recipe'`

// The first-use features that end the activation funnel.
const activationFeatures = `'terminal','exec','transfer','tunnel'`

func cur(days int) string  { return fmt.Sprintf("timestamp >= now() - INTERVAL %d DAY", days) }
func prev(days int) string { return fmt.Sprintf("timestamp < now() - INTERVAL %d DAY", days) }
func window(days int) string {
	return fmt.Sprintf("timestamp >= now() - INTERVAL %d DAY", 2*days)
}

func pairQuery(days int, where string, what string) string {
	return fmt.Sprintf(`SELECT uniqIf(distinct_id, %s AND %s), uniqIf(distinct_id, %s AND %s) FROM events WHERE %s AND %s`,
		what, cur(days), what, prev(days), where, window(days))
}

// sourceQuery counts sessions by the first page's referrer domain or campaign, and the sessions that clicked sign-up.
func sourceQuery(days int, expr string) string {
	return `SELECT name, count() AS visits, countIf(clicks > 0) AS signup_clicks FROM (
 SELECT properties.$session_id AS sid, argMinIf(coalesce(toString(` + expr + `), ''), timestamp, event='$pageview') AS name, countIf(event='signup_clicked') AS clicks
 FROM events WHERE properties.app='site' AND event IN ('$pageview','signup_clicked') AND ` + cur(days) + ` GROUP BY sid)
 WHERE name != '' GROUP BY name ORDER BY signup_clicks DESC, visits DESC LIMIT 5`
}

func siteCountQuery(days int, expr string) string {
	return `SELECT coalesce(toString(` + expr + `), '') AS name, uniq(distinct_id) AS n FROM events
 WHERE event='$pageview' AND properties.app='site' AND ` + cur(days) + ` GROUP BY name HAVING name != '' ORDER BY n DESC LIMIT 5`
}

func onlineQuery(app string) string {
	return `SELECT uniq(distinct_id) FROM events WHERE properties.app IN (` + app + `) AND timestamp >= now() - INTERVAL 5 MINUTE`
}

// signups are accounts whose first signed_in (the server's event) falls in the range.
func signupsQuery(days int) string {
	return fmt.Sprintf(`SELECT countIf(first >= now() - INTERVAL %d DAY), countIf(first < now() - INTERVAL %d DAY) FROM (
 SELECT distinct_id, min(timestamp) AS first FROM events WHERE event='signed_in' AND properties.app='server' GROUP BY distinct_id HAVING first >= now() - INTERVAL %d DAY)`, days, days, 2*days)
}

func dailyQuery(days int) string {
	return `SELECT toString(toDate(timestamp)) AS day, uniq(distinct_id) FROM events WHERE event='$pageview' AND properties.app='site' AND ` +
		cur(days) + ` GROUP BY day ORDER BY day`
}

const activeApps = `'console','server'`

func activeQuery() string {
	return `SELECT uniqIf(distinct_id, timestamp >= now() - INTERVAL 1 DAY), uniqIf(distinct_id, timestamp >= now() - INTERVAL 2 DAY AND timestamp < now() - INTERVAL 1 DAY),
 uniqIf(distinct_id, timestamp >= now() - INTERVAL 7 DAY), uniqIf(distinct_id, timestamp >= now() - INTERVAL 14 DAY AND timestamp < now() - INTERVAL 7 DAY)
 FROM events WHERE properties.app IN (` + activeApps + `) AND timestamp >= now() - INTERVAL 14 DAY`
}

// funnelQuery follows the accounts that signed up in the range, wherever their later steps fall.
func funnelQuery(days int) string {
	return fmt.Sprintf(`SELECT count(), countIf(machines > 0), countIf(machines > 0 AND used > 0) FROM (
 SELECT distinct_id, minIf(timestamp, event='signed_in') AS signup, countIf(event='machine_enrolled') AS machines,
  countIf(event='feature_used' AND properties.feature IN (%s)) AS used
 FROM events WHERE properties.app='server' GROUP BY distinct_id HAVING signup >= now() - INTERVAL %d DAY)`, activationFeatures, days)
}

func retentionQuery(days int) string {
	return fmt.Sprintf(`SELECT signup >= now() - INTERVAL %d DAY AS recent, count(), countIf(last_active >= signup + INTERVAL 7 DAY) FROM (
 SELECT distinct_id, minIf(timestamp, event='signed_in' AND properties.app='server') AS signup, max(timestamp) AS last_active
 FROM events WHERE properties.app IN (%s) GROUP BY distinct_id
 HAVING signup >= now() - INTERVAL %d DAY AND signup <= now() - INTERVAL 7 DAY) GROUP BY recent`, days, activeApps, 2*days)
}

func adoptionQuery(days int) string {
	return `SELECT toString(properties.feature) AS feature, uniq(distinct_id) AS n FROM events WHERE event='feature_used' AND properties.app='server' AND properties.feature IN (` +
		adoptionFeatures + `) AND ` + cur(days) + ` GROUP BY feature ORDER BY n DESC`
}

func activeAccountsQuery(days int) string {
	return `SELECT uniq(distinct_id) FROM events WHERE properties.app IN (` + activeApps + `) AND ` + cur(days)
}

func counts[T any](dst *[]T, width int, row func([]any) T) func([][]any) error {
	return func(rows [][]any) error {
		for _, r := range rows {
			if len(r) != width {
				return errors.New("unexpected column count")
			}
			*dst = append(*dst, row(r))
		}
		return nil
	}
}

func one(width int, set func([]any)) func([][]any) error {
	return func(rows [][]any) error {
		if len(rows) != 1 || len(rows[0]) != width {
			return errors.New("unexpected result shape")
		}
		set(rows[0])
		return nil
	}
}

// Site returns the marketing-site insights for the last `days` days (0 means DefaultDays).
func (s *Service) Site(ctx context.Context, days int) (Site, error) {
	days, err := s.checkRange(days)
	if err != nil {
		return Site{}, err
	}
	value, err := s.cachedValue(fmt.Sprint("site/", days), cacheTTL, func() (any, error) { return s.fetchSite(ctx, days) })
	if err != nil {
		return Site{}, err
	}
	out := value.(Site)
	out.Online, _ = s.online(ctx, "'site'")
	return out, nil
}

// online is the visitors with an event in the last five minutes, read fresh every half minute.
func (s *Service) online(ctx context.Context, apps string) (int64, error) {
	value, err := s.cachedValue("online/"+apps, onlineTTL, func() (any, error) {
		rows, err := s.query(ctx, onlineQuery(apps))
		if err != nil || len(rows) != 1 || len(rows[0]) != 1 {
			return nil, errors.New("online count unavailable")
		}
		return toInt(rows[0][0]), nil
	})
	if err != nil {
		return 0, err
	}
	return value.(int64), nil
}

func (s *Service) fetchSite(ctx context.Context, days int) (Site, error) {
	out := Site{ObservedAt: s.now().UTC(), Days: days, URL: s.projectURL("/web"), Daily: []Day{}, Referrers: []Source{}, Campaigns: []Source{}, Pages: []Count{}, Countries: []Count{}}
	source := func(r []any) Source { return Source{fmt.Sprint(r[0]), toInt(r[1]), toInt(r[2])} }
	count := func(r []any) Count { return Count{fmt.Sprint(r[0]), toInt(r[1])} }
	jobs := []job{
		{"visitors", pairQuery(days, "properties.app='site'", "event='$pageview'"), one(2, func(r []any) { out.Visitors = Pair{toInt(r[0]), toInt(r[1])} })},
		{"signups", signupsQuery(days), one(2, func(r []any) { out.Signups = Pair{toInt(r[0]), toInt(r[1])} })},
		{"daily", dailyQuery(days), counts(&out.Daily, 2, func(r []any) Day { return Day{fmt.Sprint(r[0]), toInt(r[1])} })},
		{"referrers", sourceQuery(days, "properties.$referring_domain"), counts(&out.Referrers, 3, source)},
		{"campaigns", sourceQuery(days, "properties.utm_campaign"), counts(&out.Campaigns, 3, source)},
		{"pages", siteCountQuery(days, "properties.$pathname"), counts(&out.Pages, 2, count)},
		{"countries", siteCountQuery(days, "properties.$geoip_country_code"), counts(&out.Countries, 2, count)},
	}
	failures, err := s.runJobs(ctx, jobs)
	out.Errors = failures
	return out, err
}

// Console returns the product insights for the last `days` days (0 means DefaultDays).
func (s *Service) Console(ctx context.Context, days int) (Console, error) {
	days, err := s.checkRange(days)
	if err != nil {
		return Console{}, err
	}
	value, err := s.cachedValue(fmt.Sprint("console/", days), cacheTTL, func() (any, error) { return s.fetchConsole(ctx, days) })
	if err != nil {
		return Console{}, err
	}
	out := value.(Console)
	out.Online, _ = s.online(ctx, "'console'")
	return out, nil
}

func (s *Service) fetchConsole(ctx context.Context, days int) (Console, error) {
	out := Console{ObservedAt: s.now().UTC(), Days: days, URL: s.projectURL("/activity/explore"), Funnel: []FunnelStep{}, Adoption: []Count{}}
	var signups, machines, used int64
	jobs := []job{
		{"active", activeQuery(), one(4, func(r []any) {
			out.DailyActive, out.WeeklyActive = Pair{toInt(r[0]), toInt(r[1])}, Pair{toInt(r[2]), toInt(r[3])}
		})},
		{"paid", pairQuery(days, "properties.app='console'", "event='checkout_completed'"), one(2, func(r []any) { out.Paid = Pair{toInt(r[0]), toInt(r[1])} })},
		{"active_accounts", activeAccountsQuery(days), one(1, func(r []any) { out.ActiveAccounts = toInt(r[0]) })},
		{"funnel", funnelQuery(days), one(3, func(r []any) { signups, machines, used = toInt(r[0]), toInt(r[1]), toInt(r[2]) })},
		{"retention", retentionQuery(days), func(rows [][]any) error {
			for _, r := range rows {
				if len(r) != 3 {
					return errors.New("unexpected column count")
				}
				if toInt(r[0]) == 1 {
					out.Retention.Size, out.Retention.Retained = toInt(r[1]), toInt(r[2])
				} else {
					out.Retention.PreviousSize, out.Retention.PreviousRetained = toInt(r[1]), toInt(r[2])
				}
			}
			return nil
		}},
		{"adoption", adoptionQuery(days), counts(&out.Adoption, 2, func(r []any) Count { return Count{fmt.Sprint(r[0]), toInt(r[1])} })},
	}
	failures, err := s.runJobs(ctx, jobs)
	out.Errors = failures
	if signups > 0 {
		out.Funnel = []FunnelStep{{"Signed up", signups}, {"Added a machine", machines}, {"Used a terminal, command, transfer or tunnel", used}}
	}
	return out, err
}
