package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
	"github.com/protobuf-orm/ent/dialect"

	"github.com/lesomnus/cr/blob"
	"github.com/lesomnus/cr/cmd"
	"github.com/lesomnus/cr/importer"
)

// NewCmdImport is `cr import SRC`: an OCI image layout, or a directory of
// them as zot keeps its root, taken into the store by hard link.
func NewCmdImport(c *cmd.Config) *xli.Command {
	return &xli.Command{
		Name:  "import",
		Brief: "take OCI image layouts in, by hard link, tags included",

		Args: arg.Args{&arg.String{Name: "SRC"}},
		Flags: flg.Flags{
			&flg.String{Name: "repo", Brief: "the repository a single layout is taken in as"},
			&flg.String{Name: "only", Brief: "the repositories to take, separated by commas; every one when empty"},
			&flg.String{Name: "exclude", Brief: "prefixes of repositories to leave, separated by commas"},
			&flg.Switch{Name: "verify", Brief: "hash every blob, and refuse one that is not its digest"},
			&flg.Switch{Name: "copy", Brief: "copy a blob that cannot be linked, rather than stop"},
			&flg.Switch{Name: "offline", Brief: "on SQLite, say that no server is running on this database"},
		},

		Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
			src, _ := arg.Get[string](self, "SRC")
			repo, _ := flg.Find[string](self, "repo")
			only, _ := flg.Find[string](self, "only")
			exclude, _ := flg.Find[string](self, "exclude")
			verify, _ := flg.Find[bool](self, "verify")
			copy_, _ := flg.Find[bool](self, "copy")
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

			// The index is written under the repository lock, which on SQLite
			// is the serving process's.
			if s.Dialect != dialect.Postgres && !offline {
				return errors.New("on SQLite the repository lock is the serving process's: stop it and pass --offline")
			}

			stores, err := Stores(c.Registry.Storage, meterOf(ctx))
			if err != nil {
				return err
			}
			// A pull-through cache holds what its upstream has, and nothing
			// is pushed into one; nothing is imported into one either.
			refuse := func(repo string) string {
				for _, p := range c.Registry.Proxies {
					if blob.Covers(p.Prefix, repo) {
						return "a pull-through cache of " + p.Upstream
					}
				}
				return ""
			}

			r, err := importer.Import(ctx, src, stores, ix, importer.Options{
				Repo:            repo,
				Only:            commaList(only),
				Exclude:         commaList(exclude),
				Refuse:          refuse,
				Verify:          verify,
				Copy:            copy_,
				MaxManifestSize: c.Registry.MaxManifestSize,
			})
			printJSON(self, r)
			return err
		}),
	}
}

func commaList(s string) []string {
	var out []string
	for v := range strings.SplitSeq(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
