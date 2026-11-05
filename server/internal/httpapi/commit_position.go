package httpapi

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// commitPositionHeader carries the primary's WAL position after a write and the highest position a client has
// seen on later reads (read-your-writes on regional replicas, PLAN 3b).
const commitPositionHeader = "Paperboat-Commit-Position"

var lsnPattern = regexp.MustCompile(`^[0-9A-F]{1,8}/[0-9A-F]{1,8}$`)

type commitPositionKey struct{}

// commitPosition records the client's position for replica reads and, on a successful write, answers with the
// primary's position. A malformed position is treated as unknown, so that client's reads go to the primary.
func commitPosition(d *db.DB, enabled bool, next http.Handler) http.Handler {
	if d == nil || !enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		position := r.Header.Get(commitPositionHeader)
		if position != "" && !lsnPattern.MatchString(position) {
			position = "FFFFFFFF/FFFFFFFF" // no replica reaches it, so the primary answers
		}
		r = r.WithContext(context.WithValue(r.Context(), commitPositionKey{}, position))
		if !unsafeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(&commitPositionWriter{ResponseWriter: w, db: d, ctx: r.Context()}, r)
	})
}

type commitPositionWriter struct {
	http.ResponseWriter
	db      *db.DB
	ctx     context.Context
	written bool
}

func (w *commitPositionWriter) WriteHeader(status int) {
	if !w.written {
		w.written = true
		if status < 400 {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), time.Second)
			if lsn, err := w.db.CommitPosition(ctx); err == nil {
				w.Header().Set(commitPositionHeader, lsn)
			}
			cancel()
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *commitPositionWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *commitPositionWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// replicaRead is the pool for a replica-safe read in this request (see db.ReadPool for what may use it).
func replicaRead(r *http.Request, d *db.DB) *pgxpool.Pool {
	position, _ := r.Context().Value(commitPositionKey{}).(string)
	return d.ReadPool(r.Context(), position)
}
