package cli

import (
	"context"
	"net"

	entschema "github.com/protobuf-orm/ent/dialect/sql/schema"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pdauth "github.com/lesomnus/payday/auth"
	"github.com/lesomnus/payday/pdcmd"

	"github.com/lesomnus/cr/cmd"
	entmigrate "github.com/lesomnus/cr/internal/ent/migrate"
)

// local is the management API served inside the command that uses it: the
// same stack `serve` runs, on the same database, over a pipe.
//
// It is how `cr binding add` and the rest reach the rows on the host where the
// database is, with no token to hand out first. The caller is whoever
// `management.as` names, believed as `Plain` believes: somebody who can open
// the database could write the rows without asking, so the pipe grants nothing
// they did not already have, and the trail still says who it was.
type local struct {
	c *cmd.Config
}

func (l *local) Connect(ctx context.Context) (pdcmd.Conn, func(), error) {
	s, err := cmd.Build(ctx, *l.c)
	if err != nil {
		return nil, nil, err
	}
	if l.c.Db.Migrate {
		if err := Migrate(ctx, s); err != nil {
			s.Close()
			return nil, nil, err
		}
	} else if err := entschema.Check(ctx, s.Db, s.Dialect, entmigrate.Tables); err != nil {
		s.Close()
		return nil, nil, err
	}

	g, err := s.Grpc(ctx, *l.c)
	if err != nil {
		s.Close()
		return nil, nil, err
	}
	lis := bufconn.Listen(1 << 20)
	go g.Serve(lis)

	as := l.c.Management.As
	if as == "" {
		as = "@operator/admin"
	}
	opts := []grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	opts = append(opts, pdauth.Inject(pdauth.PlainProvider(as))...)
	conn, err := grpc.NewClient("passthrough:///cr", opts...)
	if err != nil {
		g.Stop()
		s.Close()
		return nil, nil, err
	}
	return conn, func() {
		conn.Close()
		g.Stop()
		s.Close()
	}, nil
}
