package entindex_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/cr/index/entindex"
	"github.com/lesomnus/cr/internal/ent"
	"github.com/lesomnus/cr/internal/ent/orphanscan"
	"github.com/lesomnus/payday/config"
	"github.com/opencontainers/go-digest"
	entsql "github.com/protobuf-orm/ent/dialect/sql"
	"github.com/stretchr/testify/require"
)

func TestSqliteOrphanCheckpointRestart(t *testing.T) {
	ctx := context.Background()
	cfg := config.DbConfig{Driver: "sqlite3", Dsn: "file:" + filepath.Join(t.TempDir(), "restart.db") + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"}
	ix := open(t, cfg, 0)
	require.NoError(t, ix.Tx(ctx, "repo", func(tx index.Index) error {
		for i := range 501 {
			m := index.Manifest{Digest: digest.FromString(fmt.Sprintf("row-%d", i)), MediaType: "application/vnd.oci.image.manifest.v1+json"}
			if err := tx.Manifest().Put(ctx, "repo", m, nil); err != nil {
				return err
			}
		}
		return nil
	}))
	done, err := ix.Orphan().Seed(ctx, "repo")
	require.NoError(t, err)
	require.False(t, done)

	// A separate client sees the durable cursor and finishes the remaining
	// batch. Log subsequent queries to prove completed scans do not read rows.
	db, dialect, err := cfg.Open(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	var mu sync.Mutex
	var queries []string
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect, db)), ent.Debug(), ent.Log(func(args ...any) { mu.Lock(); defer mu.Unlock(); queries = append(queries, fmt.Sprint(args...)) }))
	restarted := entindex.New(client)
	done, err = restarted.Orphan().Seed(ctx, "repo")
	require.NoError(t, err)
	require.True(t, done)
	mu.Lock()
	queries = nil
	mu.Unlock()
	done, err = restarted.Orphan().Seed(ctx, "repo")
	require.NoError(t, err)
	require.True(t, done)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, queries, 1, "completed scans need only the checkpoint lookup")
	require.Contains(t, queries[0], orphanscan.Table)
	require.False(t, strings.Contains(queries[0], "FROM `manifest`"))
}

func TestSqliteOrphanCandidateSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	cfg := config.DbConfig{Driver: "sqlite3", Dsn: "file:" + filepath.Join(t.TempDir(), "candidate.db")}
	ix := open(t, cfg, 0)
	subject := digest.FromString("missing")
	m := index.Manifest{Digest: digest.FromString("referrer"), Subject: subject, MediaType: "application/vnd.oci.image.manifest.v1+json", CreatedAt: time.Now().Add(-time.Hour)}
	require.NoError(t, ix.Manifest().Put(ctx, "repo", m, nil))
	before, err := ix.Orphan().Get(ctx, "repo", subject)
	require.NoError(t, err)
	reopened := open(t, cfg, 0)
	after, err := reopened.Orphan().Get(ctx, "repo", subject)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
