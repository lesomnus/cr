package entindex

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/protobuf-orm/ent/dialect"

	"github.com/lesomnus/cr/index"
)

// db is the pool under the client, for what needs a connection of its own.
func (ix *Index) db() (*sql.DB, error) {
	drv := ix.client.Driver()
	for {
		switch v := drv.(type) {
		case interface{ DB() *sql.DB }:
			return v.DB(), nil
		case *dialect.DebugDriver:
			drv = v.Driver
		default:
			return nil, fmt.Errorf("entindex: %T has no connection pool", drv)
		}
	}
}

// Lock takes repo's lock outside any transaction and holds it until the
// answered function is called: the lock a manifest write's transaction takes,
// held across work that is not a transaction, the sweep's walk of a store.
//
// On PostgreSQL it is the session form of the same advisory lock, on a
// connection of its own. Elsewhere it is the process's lock, and it does not
// stop other repositories' writers, only this one's.
func (ix *Index) Lock(ctx context.Context, repo string) (func(), error) {
	if ix.dialect != dialect.Postgres {
		return ix.acquire(ctx, ix.o.stripes[key(repo)%uint64(len(ix.o.stripes))])
	}

	db, err := ix.db()
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if ix.o.wait > 0 {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET lock_timeout = %d", ix.o.wait.Milliseconds())); err != nil {
			conn.Close()
			return nil, err
		}
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", int64(key(repo))); err != nil {
		conn.Close()
		if strings.Contains(err.Error(), "55P03") || strings.Contains(err.Error(), "lock timeout") {
			return nil, index.ErrBusy
		}
		return nil, err
	}
	return func() {
		ctx := context.WithoutCancel(ctx)
		conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", int64(key(repo)))
		conn.ExecContext(ctx, "RESET lock_timeout")
		conn.Close()
	}, nil
}

// Lead runs fn if no other process is running the work called name, and
// reports whether it did: one runner for the background work every replica
// schedules. On anything but PostgreSQL there is one process, and it leads.
func (ix *Index) Lead(ctx context.Context, name string, fn func(context.Context) error) (bool, error) {
	if ix.dialect != dialect.Postgres {
		return true, fn(ctx)
	}

	db, err := ix.db()
	if err != nil {
		return false, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	k := int64(key("\x00lead/" + name))
	var won bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", k).Scan(&won); err != nil {
		return false, err
	}
	if !won {
		return false, nil
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", k)
	return true, fn(ctx)
}
