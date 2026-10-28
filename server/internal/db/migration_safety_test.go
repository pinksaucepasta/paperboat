package db

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// This is a statement-level guard, not a PostgreSQL parser. Quoted bodies and
// literals are opaque so function definitions are not mistaken for backfills.
var migrationTokens = regexp.MustCompile(`(?s)--[^\n]*|/\*.*?\*/|'(?:''|[^'])*'|"(?:""|[^"])*"|\$[\w]*\$|;|[^\s;'"$/-]+|.`)
var migrationDDL = regexp.MustCompile(`^(ALTER|CREATE|DROP|TRUNCATE|COMMENT|REINDEX|GRANT|REVOKE)\b`)
var migrationDML = regexp.MustCompile(`\b(INSERT\s+INTO|UPDATE\s+\S+\s+SET|DELETE\s+FROM|MERGE\s+INTO)\b`)
var migrationConstraint = regexp.MustCompile(`\bADD\s+(?:CONSTRAINT\s+\S+\s+)?(?:FOREIGN\s+KEY|CHECK)\b`)
var migrationAdd = regexp.MustCompile(`\bADD\s+`)

func migrationStatements(body string) []string {
	var statements []string
	var statement strings.Builder
	dollar := ""
	for _, token := range migrationTokens.FindAllString(body, -1) {
		if dollar != "" {
			if token == dollar {
				dollar = ""
			}
			continue
		}
		switch {
		case strings.HasPrefix(token, "--"), strings.HasPrefix(token, "/*"):
			statement.WriteByte(' ')
		case strings.HasPrefix(token, "$") && strings.HasSuffix(token, "$") && len(token) >= 2:
			dollar = token
			statement.WriteString(" body ")
		case strings.HasPrefix(token, "'"), strings.HasPrefix(token, `"`):
			statement.WriteString(" quoted ")
		case token == ";":
			if s := strings.TrimSpace(statement.String()); s != "" {
				statements = append(statements, strings.ToUpper(s))
			}
			statement.Reset()
		default:
			statement.WriteString(token)
		}
	}
	if s := strings.TrimSpace(statement.String()); s != "" {
		statements = append(statements, strings.ToUpper(s))
	}
	return statements
}

func migrationSafetyErrors(body string) []string {
	statements := migrationStatements(body)
	var problems []string
	ddl, dml := false, false
	for _, statement := range statements {
		ddl = ddl || migrationDDL.MatchString(statement)
		dml = dml || migrationDML.MatchString(statement)
		for _, clause := range migrationAdd.Split(statement, -1)[1:] {
			if migrationConstraint.MatchString("ADD "+clause) && !strings.Contains(clause, "NOT VALID") {
				problems = append(problems, "add FOREIGN KEY/CHECK as NOT VALID; validate in a later migration")
			}
		}
	}
	if ddl && dml {
		problems = append(problems, "separate DDL and data DML migrations")
	}
	if ddl && (len(statements) == 0 || !regexp.MustCompile(`^SET\s+(?:LOCAL\s+|SESSION\s+)?LOCK_TIMEOUT\s*(?:=|TO)\s+\S+`).MatchString(statements[0])) {
		problems = append(problems, "start DDL migrations with SET lock_timeout")
	}
	return problems
}

func TestMigrationLiveDatabaseSafety(t *testing.T) {
	entries, err := migrationsFS.ReadDir(migrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if err != nil {
			t.Fatal(err)
		}
		if version <= 211 {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			body, err := migrationsFS.ReadFile(migrationsDir + "/" + entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			for _, problem := range migrationSafetyErrors(string(body)) {
				t.Error(problem)
			}
		})
	}
}

func TestMigrationSafetySamples(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		problems  int
	}{
		{"data only", `UPDATE t SET x=1; DELETE FROM t;`, 0},
		{"ddl only", `-- +goose Up
SET lock_timeout='5s'; ALTER TABLE t ADD COLUMN x int;`, 0},
		{"constraints", `SET lock_timeout='5s'; ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (x) REFERENCES p(id) NOT VALID, ADD CONSTRAINT ck CHECK (x>0) NOT VALID;`, 0},
		{"validate later", `SET LOCAL lock_timeout='5s'; ALTER TABLE t VALIDATE CONSTRAINT ck;`, 0},
		{"mixed", `SET lock_timeout='5s'; ALTER TABLE t DROP CONSTRAINT fk; DELETE FROM t;`, 1},
		{"cte data", `SET lock_timeout='5s'; ALTER TABLE t ADD x int; WITH ids AS (SELECT id FROM t) UPDATE t SET x=1;`, 1},
		{"missing timeout", `ALTER TABLE t ADD x int;`, 1},
		{"late timeout", `ALTER TABLE t ADD x int; SET lock_timeout='5s';`, 1},
		{"foreign key", `SET lock_timeout='5s'; ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (x) REFERENCES p(id);`, 1},
		{"second constraint", `SET lock_timeout='5s'; ALTER TABLE t ADD CONSTRAINT ck CHECK (x>0) NOT VALID, ADD CONSTRAINT other CHECK (x<10);`, 1},
		{"comments and function", `/* DELETE FROM t; */ SET lock_timeout='5s'; CREATE FUNCTION f() RETURNS void AS $fn$ BEGIN DELETE FROM t; END $fn$ LANGUAGE plpgsql; -- UPDATE t SET x=1;`, 0},
		{"literal", `INSERT INTO t VALUES ('CREATE TABLE t; --');`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := migrationSafetyErrors(tc.sql); len(got) != tc.problems {
				t.Fatalf("problems = %v, want %d", got, tc.problems)
			}
		})
	}
}

func TestMigrationRetryClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"lock timeout", &pgconn.PgError{Code: "55P03"}, true},
		{"wrapped deadlock", fmt.Errorf("goose: %w", &pgconn.PgError{Code: "40P01"}), true},
		{"statement timeout", &pgconn.PgError{Code: "57014"}, false},
		{"constraint failure", &pgconn.PgError{Code: "23503"}, false},
		{"network failure", errors.New("connection reset"), false},
		{"cancelled", context.Canceled, false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryableMigrationError(tc.err); got != tc.retry {
				t.Fatalf("retry = %v, want %v", got, tc.retry)
			}
		})
	}
}
