package cli

// The drivers this app runs on in a process. They are blank-imported by the app
// rather than by payday so that an app does not carry an engine it never opens.
//
// They are in this package and not in `cmd` because a blank import is a
// property of the package that writes it, and `cmd` is what an app's sandbox
// imports for `Build`. A driver named there is linked into the page as well --
// and the SQLite one brings SQLite compiled to Wasm and run on wazero, which is
// wasm inside wasm. The page opens `config/dbsqlite3wasm` instead: SQLite in a
// Worker beside it.
import (
	// SQLite, for one process.
	_ "github.com/lesomnus/payday/config/dbsqlite3"

	// PostgreSQL, `db.driver: pgx`, for more than one; and the broker a Watch
	// needs across them, `watch.broker: postgres`.
	_ "github.com/lesomnus/payday/config/brokerpg"
	_ "github.com/lesomnus/payday/config/dbpgx"
)
