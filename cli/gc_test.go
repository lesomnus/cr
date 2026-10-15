package cli

import (
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/lesomnus/cr/cmd"
	"github.com/lesomnus/cr/index"
	"github.com/lesomnus/payday/config"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"
)

func TestGcRescanOrphans(t *testing.T) {
	for _, full := range []bool{false, true} {
		name := "online"
		if full {
			name = "full"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			c := cmd.Config{
				Db:    config.DbConfig{Driver: "sqlite3", Dsn: "file:" + filepath.Join(t.TempDir(), "cr.db"), Migrate: true},
				Watch: config.WatchConfig{Broker: config.BrokerMemory},
				Registry: cmd.RegistryConfig{
					Storage: cmd.StorageConfig{Driver: "os", Os: cmd.OsStorageConfig{Root: t.TempDir()}},
					Gc:      cmd.GcConfig{Orphaned: 24 * time.Hour},
				},
			}
			_, ix, closeAll, err := open(ctx, &c)
			require.NoError(t, err)
			t.Cleanup(closeAll)
			const repo = "acme/app"
			_, err = ix.Repo().Ensure(ctx, repo)
			require.NoError(t, err)
			missing := digest.FromString("missing")
			kept := digest.FromString("kept")
			for _, subject := range []digest.Digest{missing, kept} {
				require.NoError(t, ix.Manifest().Put(ctx, repo, index.Manifest{
					Digest: digest.FromString("referrer-" + subject.String()), Subject: subject,
					MediaType: "application/vnd.oci.image.manifest.v1+json",
				}, nil))
			}
			done, err := ix.Orphan().Seed(ctx, repo)
			require.NoError(t, err)
			require.True(t, done)
			before, err := ix.Orphan().Get(ctx, repo, kept)
			require.NoError(t, err)
			require.NoError(t, ix.Orphan().Erase(ctx, repo, missing))
			closeAll()

			run := func(args ...string) error {
				command := NewCmdGc(&c)
				command.Writer = io.Discard
				return command.Run(ctx, args)
			}
			require.ErrorContains(t, run("--rescan-orphans"), "stop it and pass --offline")
			args := []string{"--offline"}
			if full {
				args = append(args, "--full")
			}
			require.NoError(t, run(args...))
			_, ix, closeAll, err = open(ctx, &c)
			require.NoError(t, err)
			t.Cleanup(closeAll)
			_, err = ix.Orphan().Get(ctx, repo, missing)
			require.ErrorIs(t, err, index.ErrNotFound, "ordinary/full GC does not rescan")
			closeAll()

			require.NoError(t, run(append(args, "--rescan-orphans")...))
			_, ix, closeAll, err = open(ctx, &c)
			require.NoError(t, err)
			defer closeAll()
			_, err = ix.Orphan().Get(ctx, repo, missing)
			require.NoError(t, err, "the CLI restarts discovery")
			after, err := ix.Orphan().Get(ctx, repo, kept)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestGcRescanOrphansDisabled(t *testing.T) {
	for _, grace := range []time.Duration{0, -time.Hour} {
		c := cmd.Config{Registry: cmd.RegistryConfig{Gc: cmd.GcConfig{Orphaned: grace}}}
		err := NewCmdGc(&c).Run(t.Context(), []string{"--rescan-orphans", "--offline"})
		require.ErrorContains(t, err, "--rescan-orphans requires registry.gc.orphaned > 0")
	}
}
