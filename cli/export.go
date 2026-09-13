package cli

import (
	"context"
	"strings"

	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"

	"github.com/lesomnus/cr/cmd"
	"github.com/lesomnus/cr/export"
)

// NewCmdExport is `cr export REPO DIR`: a repository written out as an OCI
// image layout, which `oras`, `skopeo`, `crane` and containerd read.
func NewCmdExport(c *cmd.Config) *xli.Command {
	return &xli.Command{
		Name:  "export",
		Brief: "write a repository out as an OCI image layout",

		Args: arg.Args{&arg.String{Name: "REPO"}, &arg.String{Name: "DIR"}},
		Flags: flg.Flags{
			&flg.String{Name: "tags", Brief: "the tags to export, separated by commas; every tag when empty"},
			&flg.Switch{Name: "no-referrers", Brief: "leave out the signatures, attestations and SBOMs that refer to them"},
		},

		Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
			repo, _ := arg.Get[string](self, "REPO")
			dir, _ := arg.Get[string](self, "DIR")
			noReferrers, _ := flg.Find[bool](self, "no-referrers")
			var tags []string
			if v, ok := flg.Find[string](self, "tags"); ok {
				for t := range strings.SplitSeq(v, ",") {
					if t = strings.TrimSpace(t); t != "" {
						tags = append(tags, t)
					}
				}
			}

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
			stores, _, _, err = Proxies(c.Registry, stores)
			if err != nil {
				return err
			}

			r, err := export.Layout(ctx, ix, stores.Use(repo), repo, dir, export.Options{Tags: tags, Referrers: !noReferrers})
			printJSON(self, r)
			return err
		}),
	}
}
