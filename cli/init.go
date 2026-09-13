package cli

import (
	"context"
	"fmt"

	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"

	"github.com/lesomnus/payday/pdid"

	app "github.com/lesomnus/cr/api"
	"github.com/lesomnus/cr/cmd"
)

// NewCmdInit is `cr init`: the first tenant, and somebody in it.
//
// It exists because there is nowhere else it could happen. A tenant is not put
// up from inside one, so the first row of a deployment cannot arrive over the
// API. What puts it there is [Server.Ungated], which is not a privilege anybody
// holds: it is a server instance this process was handed, reachable from this
// command and from nowhere a request can get to.
//
// Running it twice is an error rather than a no-op, because an alias is unique
// and the database says so. That is the right answer -- an `init` that quietly
// did nothing is one somebody runs against the wrong deployment and believes.
func NewCmdInit(c *cmd.Config) *xli.Command {
	return &xli.Command{
		Name:  "init",
		Brief: "put up the first tenant and somebody in it",

		Flags: flg.Flags{
			&flg.String{Name: "tenant", Brief: "the alias of the tenant to create"},
			&flg.String{Name: "holder", Brief: "the alias of the holder to create in it"},
		},

		Handler: xli.OnRun(func(ctx context.Context, self *xli.Command, next xli.Next) error {
			tenant, ok := flg.Find[string](self, "tenant")
			if !ok || tenant == "" {
				tenant = "acme"
			}

			holder, ok := flg.Find[string](self, "holder")
			if !ok || holder == "" {
				holder = "admin"
			}

			s, err := cmd.Build(ctx, *c)
			if err != nil {
				return err
			}
			defer s.Close()

			// The schema, so that a fresh database is one this can run against.
			// A deployment with migrations of its own does that instead; see
			// payday's `migrate`.
			if err := Migrate(ctx, s); err != nil {
				return err
			}

			t, err := s.Ungated.Tenant().Add(ctx, app.TenantAddRequest_builder{
				Alias: tenant,
			}.Build())
			if err != nil {
				return fmt.Errorf("tenant %q: %w", tenant, err)
			}

			h, err := s.Ungated.Holder().Add(ctx, app.HolderAddRequest_builder{
				Tenant: app.TenantRef_builder{Id: t.GetId()}.Build(),
				Alias:  holder,
			}.Build())
			if err != nil {
				return fmt.Errorf("holder %q: %w", holder, err)
			}

			k, err := pdid.From(t.GetId())
			if err != nil {
				return err
			}
			j, err := pdid.From(h.GetId())
			if err != nil {
				return err
			}

			self.Printf("tenant %s is %s\n", tenant, k)
			self.Printf("holder %s is %s\n", holder, j)
			self.Printf("\nsign in as: @%s/%s\n", tenant, holder)

			return nil
		}),
	}
}
