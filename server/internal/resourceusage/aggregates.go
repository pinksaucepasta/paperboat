package resourceusage

import (
	"context"
	"database/sql"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// Aggregate before detail pagination. Grouping sets keep combined totals and
// path breakdowns on the same filtered measurements and UTC bucket boundary.
func aggregateUsage(ctx context.Context, tx *db.Tx, args []any, out *Result) error {
	bucket := "hour"
	if out.End.Sub(out.Start) > 48*time.Hour {
		bucket = "day"
	}
	queryArgs := append(append([]any{}, args...), bucket)
	rows, err := tx.Query(ctx, `WITH measurements AS (
 SELECT date_trunc($13,interval_end AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' bucket,
 mode,hosting,upload_bytes,download_bytes FROM resource_bandwidth_events e WHERE `+predicate+`
 ) SELECT bucket,mode,hosting,coalesce(sum(upload_bytes),0)::bigint,
 coalesce(sum(download_bytes),0)::bigint,grouping(bucket),grouping(mode,hosting)
 FROM measurements GROUP BY GROUPING SETS ((),(mode,hosting),(bucket),(bucket,mode,hosting))
 ORDER BY bucket NULLS FIRST,mode NULLS FIRST,hosting NULLS FIRST`, queryArgs...)
	if err != nil {
		return err
	}
	defer rows.Close()
	out.Totals.ByPath = []PathTotal{}
	indexes := map[time.Time]int{}
	unknown := false
	for rows.Next() {
		var bucketTime sql.NullTime
		var mode, hosting sql.NullString
		var amount Bytes
		var groupedTime, groupedPath int
		if err = rows.Scan(&bucketTime, &mode, &hosting, &amount.Upload, &amount.Download, &groupedTime, &groupedPath); err != nil {
			return err
		}
		target := &out.Totals
		if groupedTime == 0 {
			start := bucketTime.Time.UTC()
			index, ok := indexes[start]
			if !ok {
				index = len(out.Series)
				indexes[start] = index
				out.Series = append(out.Series, Point{Start: start, Totals: Totals{ByPath: []PathTotal{}}})
			}
			target = &out.Series[index].Totals
		}
		if groupedPath != 0 {
			target.Bytes = amount
			continue
		}
		if mode.String == "unknown" || hosting.String == "unknown" {
			unknown = true
			continue
		}
		target.ByPath = append(target.ByPath, PathTotal{Mode: mode.String, Hosting: hosting.String, Bytes: amount})
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if unknown {
		out.Coverage.Note += " Path and hosting breakdowns exclude measurements with unknown attribution; combined totals retain them."
	}
	return nil
}
