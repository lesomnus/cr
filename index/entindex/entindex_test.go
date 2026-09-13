package entindex_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	entsql "github.com/protobuf-orm/ent/dialect/sql"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/payday/config"
	_ "github.com/lesomnus/payday/config/dbpgx"
	_ "github.com/lesomnus/payday/config/dbsqlite3"

	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/index/entindex"
	"github.com/lesomnus/cr/index/indextest"
	"github.com/lesomnus/cr/internal/ent"
	entmigrate "github.com/lesomnus/cr/internal/ent/migrate"
)

var seq atomic.Int64

func open(t *testing.T, c config.DbConfig, wait time.Duration) *entindex.Index {
	ctx := context.Background()
	db, dialect, err := c.Open(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	drv := entsql.OpenDB(dialect, db)
	require.NoError(t, entmigrate.NewSchema(drv).Create(ctx))

	return entindex.New(ent.NewClient(ent.Driver(drv)), entindex.WithWait(wait))
}

func openSqlite(t *testing.T, wait time.Duration) *entindex.Index {
	return open(t, config.DbConfig{
		Driver: "sqlite3",
		Dsn:    fmt.Sprintf("file:%s/index-%d.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)", t.TempDir(), seq.Add(1)),
	}, wait)
}

// openPostgres is a schema of its own on CR_TEST_POSTGRES, so every test starts
// from nothing and none of them sees another.
func openPostgres(t *testing.T, wait time.Duration) *entindex.Index {
	dsn := os.Getenv("CR_TEST_POSTGRES")
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("t%d_%d", time.Now().UnixNano(), seq.Add(1))
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
	return open(t, config.DbConfig{Driver: "pgx", Dsn: dsn + sep + "search_path=" + schema}, wait)
}

func TestSqlite(t *testing.T) {
	indextest.Run(t, func(t *testing.T, wait time.Duration) index.Index { return openSqlite(t, wait) })
	t.Run("other repository", func(t *testing.T) { testOtherRepository(t, openSqlite(t, 100*time.Millisecond)) })
}

func TestPostgres(t *testing.T) {
	if os.Getenv("CR_TEST_POSTGRES") == "" {
		t.Skip("CR_TEST_POSTGRES is not set")
	}
	indextest.Run(t, func(t *testing.T, wait time.Duration) index.Index { return openPostgres(t, wait) })
	t.Run("other repository", func(t *testing.T) { testOtherRepository(t, openPostgres(t, 100*time.Millisecond)) })
	t.Run("lead", func(t *testing.T) { testLead(t, openPostgres(t, 0)) })
}

// testOtherRepository is a lock that keeps out its own repository's writers
// and not another's. Not in the shared suite: the memory index has one lock.
func testOtherRepository(t *testing.T, ix *entindex.Index) {
	ctx := context.Background()
	unlock, err := ix.Lock(ctx, "r")
	require.NoError(t, err)
	defer unlock()

	require.NoError(t, ix.Tx(ctx, "an-other-repository", func(tx index.Index) error {
		_, err := tx.Repo().Ensure(ctx, "an-other-repository")
		return err
	}))
}

func testLead(t *testing.T, ix *entindex.Index) {
	ctx := context.Background()
	inner := false
	won, err := ix.Lead(ctx, "work", func(ctx context.Context) error {
		// The same work, asked for while it runs, is somebody else's turn.
		other, err := ix.Lead(ctx, "work", func(context.Context) error { inner = true; return nil })
		require.NoError(t, err)
		require.False(t, other)
		return nil
	})
	require.NoError(t, err)
	require.True(t, won)
	require.False(t, inner)

	won, err = ix.Lead(ctx, "work", func(context.Context) error { return nil })
	require.NoError(t, err)
	require.True(t, won)
}
