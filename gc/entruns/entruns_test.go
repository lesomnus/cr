package entruns_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	entsql "github.com/protobuf-orm/ent/dialect/sql"
	"github.com/stretchr/testify/require"

	"github.com/lesomnus/payday/config"
	_ "github.com/lesomnus/payday/config/dbsqlite3"

	"github.com/lesomnus/cr/gc"
	"github.com/lesomnus/cr/gc/entruns"
	"github.com/lesomnus/cr/internal/ent"
	entmigrate "github.com/lesomnus/cr/internal/ent/migrate"
)

func TestRuns(t *testing.T) {
	ctx := context.Background()
	c := config.DbConfig{
		Driver: "sqlite3",
		Dsn:    fmt.Sprintf("file:%s/runs.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)", t.TempDir()),
	}
	db, dialect, err := c.Open(ctx)
	require.NoError(t, err)
	defer db.Close()
	drv := entsql.OpenDB(dialect, db)
	require.NoError(t, entmigrate.NewSchema(drv).Create(ctx))
	runs := entruns.New(ent.NewClient(ent.Driver(drv)))

	first, err := runs.Start(ctx, gc.KindOnline, gc.TriggerSchedule)
	require.NoError(t, err)
	require.Equal(t, gc.StateRunning, first.State)
	require.Nil(t, first.Finished)

	time.Sleep(5 * time.Millisecond)
	second, err := runs.Start(ctx, gc.KindFull, gc.TriggerAdmin)
	require.NoError(t, err)

	done := time.Now().UTC().Truncate(time.Second)
	second.State = gc.StateDone
	second.Repositories, second.Blobs, second.Bytes = 2, 3, 1024
	second.Missing = []string{"acme/app@sha256:0000"}
	second.Finished = &done
	require.NoError(t, runs.Finish(ctx, second))

	got, err := runs.Get(ctx, second.ID)
	require.NoError(t, err)
	require.Equal(t, gc.StateDone, got.State)
	require.Equal(t, 3, got.Blobs)
	require.Equal(t, int64(1024), got.Bytes)
	require.Equal(t, []string{"acme/app@sha256:0000"}, got.Missing)
	require.NotNil(t, got.Finished)
	require.True(t, done.Equal(*got.Finished))

	list, err := runs.List(ctx, 10)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, second.ID, list[0].ID, "newest first")

	_, err = runs.Get(ctx, "not-an-id")
	require.ErrorIs(t, err, gc.ErrRunNotFound)
	require.ErrorIs(t, runs.Finish(ctx, gc.Run{ID: "not-an-id"}), gc.ErrRunNotFound)
}
