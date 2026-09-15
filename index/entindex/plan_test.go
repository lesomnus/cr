package entindex_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	entsql "github.com/protobuf-orm/ent/dialect/sql"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/payday/config"

	entmigrate "github.com/lesomnus/cr/internal/ent/migrate"
)

// The plans behind the tag queries the registry sends, on the shape of the
// tables and nothing else: SQLite has no statistics unless something runs
// ANALYZE, and PostgreSQL plans a cached statement generically once it has
// run it a few times. Each query has an index that answers it whole, and
// these check the planner takes it.

func openRaw(t *testing.T, c config.DbConfig) *sql.DB {
	ctx := context.Background()
	db, dialect, err := c.Open(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, entmigrate.NewSchema(entsql.OpenDB(dialect, db)).Create(ctx))
	return db
}

func plan(t *testing.T, db *sql.DB, q string, args ...any) string {
	rows, err := db.Query(q, args...)
	require.NoError(t, err)
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	var b strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		fmt.Fprintf(&b, "%v\n", vals[len(vals)-1])
	}
	require.NoError(t, rows.Err())
	return b.String()
}

func TestSqlitePlans(t *testing.T) {
	db := openRaw(t, config.DbConfig{
		Driver: "sqlite3",
		Dsn:    fmt.Sprintf("file:%s/plans.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)", t.TempDir()),
	})
	for _, tc := range []struct {
		name, query, index string
	}{
		{"Tags.Of", "SELECT * FROM tag WHERE repo = ? AND digest = ? ORDER BY name", "tag_repo_digest_name"},
		{"Tags.Newest", "SELECT * FROM tag WHERE repo = ? ORDER BY date_moved DESC LIMIT 1", "tag_repo_date_moved"},
		{"management list", "SELECT * FROM tag WHERE repo = ? ORDER BY date_created, id LIMIT 21", "tag_repo_date_created_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []any{"r", "sha256:x"}[:strings.Count(tc.query, "?")]
			p := plan(t, db, "EXPLAIN QUERY PLAN "+tc.query, args...)
			require.Contains(t, p, tc.index, p)
			require.NotContains(t, p, "TEMP B-TREE", p)
		})
	}
}

func TestPostgresPlans(t *testing.T) {
	dsn := os.Getenv("CR_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("CR_TEST_POSTGRES is not set")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("plans%d", seq.Add(1))
	_, err = admin.Exec("CREATE SCHEMA " + schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		admin.Close()
	})
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db := openRaw(t, config.DbConfig{Driver: "pgx", Dsn: dsn + sep + "search_path=" + schema})

	// One connection, so the prepared statements and the plan cache mode
	// are the ones the EXPLAIN sees; the generic plan is the one a cached
	// statement ends up with, whatever the repository's size. Rows and
	// statistics first: on an empty table every plan is cheap, and the plan
	// that matters is the one for a small repository in a large table.
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	for _, q := range []string{
		`INSERT INTO tag (id, name, repo, digest, date_moved, date_updated, date_created)
		 SELECT gen_random_uuid(), 'build-' || i, 'r', 'sha256:' || md5(i::text), now(), now(), now()
		 FROM generate_series(1, 2000) i`,
		`INSERT INTO tag (id, name, repo, digest, date_moved, date_updated, date_created)
		 SELECT gen_random_uuid(), 'v' || i, 'tiny', 'sha256:' || md5('t' || i::text), now(), now(), now()
		 FROM generate_series(1, 5) i`,
		"ANALYZE tag",
		"SET plan_cache_mode = force_generic_plan",
	} {
		_, err := conn.ExecContext(ctx, q)
		require.NoError(t, err)
	}

	for i, tc := range []struct {
		name, query, index string
	}{
		{"Tags.Of", "SELECT * FROM tag WHERE repo = $1 AND digest = $2 ORDER BY name", "tag_repo_digest_name"},
		{"Tags.Newest", "SELECT * FROM tag WHERE repo = $1 ORDER BY date_moved DESC LIMIT 1", "tag_repo_date_moved"},
		{"management list", "SELECT * FROM tag WHERE repo = $1 ORDER BY date_created, id LIMIT 21", "tag_repo_date_created_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("q%d", i)
			params := "(text)"
			args := "('tiny')"
			if strings.Contains(tc.query, "$2") {
				params = "(text, text)"
				args = "('tiny', 'sha256:x')"
			}
			_, err := conn.ExecContext(ctx, "PREPARE "+name+params+" AS "+tc.query)
			require.NoError(t, err)
			rows, err := conn.QueryContext(ctx, "EXPLAIN EXECUTE "+name+args)
			require.NoError(t, err)
			defer rows.Close()
			var b strings.Builder
			for rows.Next() {
				var line string
				require.NoError(t, rows.Scan(&line))
				b.WriteString(line + "\n")
			}
			p := b.String()
			require.Contains(t, p, tc.index, p)
			require.NotContains(t, p, "Sort", p)
		})
	}
}
