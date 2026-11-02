package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lesomnus/cr/api"
	"github.com/lesomnus/cr/cmd"
	"github.com/lesomnus/payday/config"
	"github.com/stretchr/testify/require"
)

// A fresh database cannot expose an upgrade that discards the old trail or
// cannot add the new columns to a table with rows. Keep a pre-upgrade database
// from the real init command and exercise the same migration serve uses.
func TestMigrateExistingDatabase(t *testing.T) {
	ctx := t.Context()
	c := cmd.Config{
		Db: config.DbConfig{
			Driver: "sqlite3",
			Dsn:    "file:" + filepath.Join(t.TempDir(), "upgrade.db") + "?_pragma=foreign_keys(1)",
		},
		Watch: config.WatchConfig{Broker: config.BrokerMemory},
	}
	db, _, err := c.Db.Open(ctx)
	require.NoError(t, err)
	b, err := os.ReadFile("testdata/pre-payday-a562edd.sql")
	require.NoError(t, err)
	// Restore the dump before enabling constraints on the new connection:
	// iterdump emits holder rows before it creates the tenant table.
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, "PRAGMA foreign_keys = OFF")
	require.NoError(t, err)
	for _, statement := range strings.Split(string(b), ";") {
		if strings.TrimSpace(statement) != "" {
			_, err = db.ExecContext(ctx, statement)
			require.NoError(t, err)
		}
	}
	require.NoError(t, db.Close())
	s, err := cmd.Build(ctx, c)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, Migrate(ctx, s))
	require.NoError(t, Migrate(ctx, s), "migration is repeatable")
	tenant, err := s.Ent.Tenant.Query().Only(ctx)
	require.NoError(t, err)
	require.Equal(t, "operator", tenant.Alias)
	holder, err := s.Ent.Holder.Query().Only(ctx)
	require.NoError(t, err)
	require.Equal(t, "admin", holder.Alias)
	require.Equal(t, tenant.Id, holder.TenantId)
	old, err := s.Ent.Audit.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, old, 2)
	for _, row := range old {
		require.NotEmpty(t, row.Value, "existing audit payload survives")
	}
	_, err = s.Ungated.Holder().Add(ctx, api.HolderAddRequest_builder{
		Tenant: api.TenantRef_builder{Id: tenant.Id[:]}.Build(), Alias: "after-upgrade",
	}.Build())
	require.NoError(t, err, "new writes and their trail work on the migrated database")
	n, err := s.Ent.Audit.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, n)
	for _, row := range old {
		got, err := s.Ent.Audit.Get(ctx, row.Id)
		require.NoError(t, err)
		require.Equal(t, row.Value, got.Value)
	}
}
