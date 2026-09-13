package entindex_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	entsql "github.com/protobuf-orm/ent/dialect/sql"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/payday/config"
	_ "github.com/lesomnus/payday/config/dbsqlite3"

	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/index/entindex"
	"github.com/lesomnus/cr/index/indextest"
	"github.com/lesomnus/cr/internal/ent"
	entmigrate "github.com/lesomnus/cr/internal/ent/migrate"
)

var seq atomic.Int64

func openSqlite(t *testing.T, wait time.Duration) index.Index {
	ctx := context.Background()
	c := config.DbConfig{
		Driver: "sqlite3",
		Dsn:    fmt.Sprintf("file:%s/index-%d.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)", t.TempDir(), seq.Add(1)),
	}
	db, dialect, err := c.Open(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	drv := entsql.OpenDB(dialect, db)
	require.NoError(t, entmigrate.NewSchema(drv).Create(ctx))

	return entindex.New(ent.NewClient(ent.Driver(drv)), entindex.WithWait(wait))
}

func TestSqlite(t *testing.T) {
	indextest.Run(t, openSqlite)
}
