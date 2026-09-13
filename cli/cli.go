// Package cli is this app's command line, and what only a process does.
//
// `cmd` is the wiring both entry points share -- `Build` is there, and an app's
// sandbox calls it to stand up the same server the process stands up. This
// package is everything on the other side of that: parsing arguments, the
// database engine a process opens, and what to do about the shape of a database
// that was already there.
//
// # Why a package and not a build tag
//
// Because the linker follows imports. A blank-imported driver or a migration
// engine named in `cmd` is linked into the sandbox whether the page can use it
// or not, and neither is small: measured on payday's own app, the wazero SQLite
// engine was 15 MB of the sandbox module and Atlas -- which `Schema.Create`
// reaches -- another 10.5 MB, out of 72 MB the browser had to download.
//
// A `//go:build !js` on each would also have done it, and is worse: a tag
// refuses to compile what would compile, and says nothing about why. What is
// true is that a process and a page need different things, and a package
// boundary is how Go says that. Nothing here is excluded from a build; the
// sandbox simply does not import it.
package cli

import (
	"context"

	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"

	"github.com/lesomnus/payday/pdcmd"

	"github.com/lesomnus/cr/cmd"
	entmigrate "github.com/lesomnus/cr/internal/ent/migrate"
)

// Cmd is this app's own command line: what payday supplies, plus whatever the
// app has of its own.
//
// `config`, `config env` and `version` are payday's -- they are the commands
// that run against a **deployment** rather than against a checkout, and every
// one of them needs something only the app can hand over. `config env` is the
// clearest: listing the variables a deployment can set means walking this
// struct, and the struct is the app's.
//
// `serve` is not among them and will not be. It is the one command whose body
// is the stack -- which layers, in which order, with the wall on which server
// -- and that is the most important thing a reader of an app can see.
//
// The configuration is read on the **root**, so it has happened whichever
// subcommand runs -- `config` prints what came out, `serve` listens on what it
// says. A command that loaded it for itself would be one more place for the
// order to be wrong.
func Cmd(c *cmd.Config) *xli.Command {
	// `cr binding add`, `cr repository ls` and the rest, for every entity the
	// schema declares, served in-process on the host's database; see `local`.
	t, err := pdcmd.New(&local{c: c})
	if err != nil {
		panic(err)
	}

	return &xli.Command{
		Name:  cmd.Name,
		Brief: "cr",

		Flags: flg.Flags{pdcmd.ConfigFlag()},

		Commands: append(xli.Commands{
			pdcmd.NewCmdVersion(),
			pdcmd.NewCmdConfig(cmd.Loader, c),
			NewCmdInit(c),
			NewCmdServe(c),
			NewCmdGc(c),
			NewCmdIndex(c),
		}, t.Commands()...),

		Handler: xli.Chain(pdcmd.Load(cmd.Loader, c), xli.RequireSubcommand()),
	}
}

// Migrate brings the database s runs on into the shape this app's schema says.
//
// It is here and not on `cmd.Server` because this is the call that links the
// migration engine, and `cmd` is what an app's sandbox imports. A sandbox does
// not migrate: there is no database there that outlives the page.
func Migrate(ctx context.Context, s *cmd.Server) error {
	return entmigrate.NewSchema(s.Drv).Create(ctx)
}
