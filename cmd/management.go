package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/lesomnus/payday/auth"
	"github.com/lesomnus/payday/frame"
	"github.com/lesomnus/payday/gate"
	"github.com/lesomnus/payday/pdid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	app "github.com/lesomnus/cr/api"
	"github.com/lesomnus/z"
)

// ConfigurationAuthority is assigned only after a configured token matched.
// Directory credentials never gain configuration resolution by naming the
// same alias: their store does not set this authority.
const ConfigurationAuthority = "cr.management.configuration"

// ManagementResolver trusts configured names even when they have no holder
// row. Existing rows supply real IDs for scope and writes; absence supplies a
// named, row-free frame, never a fabricated persistent identity.
func ManagementResolver(s app.Server, directory auth.Resolver) auth.Resolver {
	local := Resolver(s)
	return auth.ResolverFunc(func(ctx context.Context, id auth.Identity) (*frame.Frame, error) {
		if id.Authority != ConfigurationAuthority {
			return directory.Resolve(ctx, id)
		}
		f, err := local.Resolve(ctx, id)
		if err == nil {
			f.Name = id.Name()
			f.TenantName = id.Tenant
			return f, nil
		}
		if !errors.Is(err, auth.ErrNoCredential) {
			return nil, err
		}
		if id.Tenant == "" || id.Alias == "" {
			return nil, status.Errorf(codes.PermissionDenied, "configured holder %s is not known; use @tenant/alias for a principal without a row", id.Name())
		}
		f = frame.New(pdid.Nil, pdid.Nil, frame.Grant{})
		f.Name = id.Name()
		f.TenantName = id.Tenant
		t, err := s.Tenant().Get(ctx, app.TenantGetRequest_builder{Ref: app.TenantRef_builder{Alias: z.Ptr(id.Tenant)}.Build()}.Build())
		if err == nil {
			f.Tenant, err = pdid.From(t.GetId())
		}
		if err != nil && status.Code(err) != codes.NotFound {
			return nil, err
		}
		return f, nil
	})
}

// ManagementPolicy keeps row-free readers on the same policy path as stored
// holders. Every unlisted method requires a persistent actor; this also runs
// per operation inside both Batch endpoints and per projected Watch image.
func ManagementPolicy(base gate.Policy) gate.Policy { return managementPolicy{base} }

type managementPolicy struct{ base gate.Policy }

func (p managementPolicy) May(ctx context.Context, c gate.Call) error {
	if c.Actor.IsZero() && !managementRead(c.Action) {
		return status.Error(codes.PermissionDenied, fmt.Sprintf("%s requires a registered holder %s", c.Action, c.Name))
	}
	if p.base != nil {
		return p.base.May(ctx, c)
	}
	return nil
}
func (p managementPolicy) Where(ctx context.Context, c gate.Call) (frame.Tenants, error) {
	if p.base != nil {
		return p.base.Where(ctx, c)
	}
	if c.Tenant.IsZero() {
		return frame.Only(), nil
	}
	return frame.Only(c.Tenant), nil
}
func managementRead(m string) bool {
	switch m {
	case "/payday.ResourceService/Batch", "/payday.BatchService/Do", "/payday.ResourceService/Watch":
		return true
	}
	// Only the generated entity services known to this application qualify.
	// A future custom RPC must make its own decision here.
	for _, service := range []string{"Archived", "Audit", "GcRun", "Holder", "Manifest", "ManifestBlob", "Outbox", "ReferrersSnapshot", "Repository", "Tag", "TagRule", "Tenant"} {
		for _, verb := range []string{"Get", "List", "Watch"} {
			if m == "/app."+service+"Service/"+verb {
				return true
			}
		}
	}
	return false
}
