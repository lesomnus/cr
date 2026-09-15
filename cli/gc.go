package cli

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"github.com/protobuf-orm/ent/dialect"
	entschema "github.com/protobuf-orm/ent/dialect/sql/schema"

	"github.com/lesomnus/cr/auth"
	"github.com/lesomnus/cr/auth/entpolicy"
	"github.com/lesomnus/cr/cmd"
	"github.com/lesomnus/cr/gc"
	"github.com/lesomnus/cr/gc/entruns"
	"github.com/lesomnus/cr/index/entindex"
	"github.com/lesomnus/cr/index/rebuild"
	entmigrate "github.com/lesomnus/cr/internal/ent/migrate"
)

// open builds the server's database, the index over it and the stores, the
// way `serve` does, for a command that works on them directly.
func open(ctx context.Context, c *cmd.Config) (*cmd.Server, *entindex.Index, func(), error) {
	s, err := cmd.Build(ctx, *c)
	if err != nil {
		return nil, nil, nil, err
	}
	if c.Db.Migrate {
		err = Migrate(ctx, s)
	} else {
		err = entschema.Check(ctx, s.Db, s.Dialect, entmigrate.Tables)
	}
	if err != nil {
		s.Close()
		return nil, nil, nil, err
	}
	var opts []entindex.Option
	if c.Registry.LockWait > 0 {
		opts = append(opts, entindex.WithWait(c.Registry.LockWait))
	}
	return s, entindex.New(s.Ent, opts...), func() { s.Close() }, nil
}

func printJSON(self *xli.Command, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	self.Printf("%s\n", b)
}

// NewCmdGc is `cr gc`: one collection, now, from this process.
func NewCmdGc(c *cmd.Config) *xli.Command {
	return &xli.Command{
		Name:  "gc",
		Brief: "collect garbage once, now",

		Flags: flg.Flags{
			&flg.Switch{Name: "full", Brief: "also sweep every repository's store"},
			&flg.Switch{Name: "offline", Brief: "on SQLite, say that no server is running on this database"},
		},

		Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
			full, _ := flg.Find[bool](self, "full")
			offline, _ := flg.Find[bool](self, "offline")

			ctx, done, err := Telemetry(ctx, c)
			if err != nil {
				return err
			}
			defer done()

			s, ix, closeAll, err := open(ctx, c)
			if err != nil {
				return err
			}
			defer closeAll()

			// On SQLite the repository lock lives in the serving process, and
			// a collection from another one would not wait for its writes.
			if s.Dialect != dialect.Postgres && !offline {
				return errors.New("on SQLite the repository lock is the serving process's: stop it and pass --offline, or ask it with POST /admin/gc")
			}

			stores, err := Stores(c.Registry.Storage)
			if err != nil {
				return err
			}
			stores, _, cache, err := Proxies(c.Registry, stores)
			if err != nil {
				return err
			}
			policy := auth.NewPolicyStore(0, staticPolicy(c.Auth), entpolicy.New(s.Ent))
			if err := policy.Refresh(ctx); err != nil {
				return err
			}

			col := gc.New(gc.Config{
				Stores:   stores,
				Index:    ix,
				Policy:   policy.Current,
				Untagged: c.Registry.Gc.Untagged,
				Delay:    c.Registry.Gc.Delay,
				Leader:   ix,
				Runs:     entruns.New(s.Ent),
				Cache:    cache,
			})
			kind := gc.KindOnline
			if full {
				kind = gc.KindFull
			}
			run, err := col.Collect(ctx, kind, gc.TriggerCli)
			printJSON(self, run)
			return err
		}),
	}
}

// NewCmdIndex is `cr index`, what is done to the index as a whole.
func NewCmdIndex(c *cmd.Config) *xli.Command {
	return &xli.Command{
		Name:  "index",
		Brief: "work on the index of names over the store",

		Commands: xli.Commands{
			{
				Name:  "rebuild",
				Brief: "recreate the index from the store alone",

				Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
					ctx, done, err := Telemetry(ctx, c)
					if err != nil {
						return err
					}
					defer done()

					_, ix, closeAll, err := open(ctx, c)
					if err != nil {
						return err
					}
					defer closeAll()

					stores, err := Stores(c.Registry.Storage)
					if err != nil {
						return err
					}
					r, err := rebuild.Rebuild(ctx, stores, ix, c.Registry.MaxManifestSize)
					printJSON(self, r)
					return err
				}),
			},
		},

		Handler: xli.RequireSubcommand(),
	}
}
