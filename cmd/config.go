// Package cmd is this app's own wiring, and it is short on purpose.
//
// Everything that does not change from one app to the next is in payday. What
// is left is here, and it is deliberately **not** hidden behind a
// `payday.Serve(cfg)`: the stack, the order of the interceptors and which
// server the wall is on are the decisions a reader of an app most needs to be
// able to see, and a framework that hid them would be hiding the only part
// worth reading.
package cmd

import (
	"github.com/lesomnus/payday/config"
)

// Name is what this app is called, and it is the only place it is written.
// The environment prefix and the names of the configuration files are derived
// from it -- CR_DB_DSN, cr.yaml -- by the loader `cli.Cmd` makes with it, so
// there is nothing to keep in step.
const Name = "cr"

// Config is what this app is configured with.
//
// The framework cannot own this struct, since what an app is configured with is
// the app's. What it owns is the pieces: each of these is a payday type, and
// what is written here is only which of them this app has.
type Config struct {
	Server config.ServerConfig `yaml:"server"`
	Db     config.DbConfig     `yaml:"db"`
	Otel   config.OtelConfig   `yaml:"otel"`
	Watch  config.WatchConfig  `yaml:"watch"`

	// Shutdown is how `serve` stops when it is told to.
	Shutdown ShutdownConfig `yaml:"shutdown"`

	// Listeners are addresses the HTTP side -- the registry, its token
	// endpoint, the management page -- is served on besides
	// `server.http.addr`, each with TLS of its own or none: plain HTTP for a
	// push from inside, and TLS that asks for client certificates for the
	// machines a policy names by them, at once.
	Listeners []ListenerConfig `yaml:"listeners"`

	Registry   RegistryConfig   `yaml:"registry"`
	Auth       AuthConfig       `yaml:"auth"`
	Management ManagementConfig `yaml:"management"`

	// From is the file this configuration was read from, and empty when
	// there was none: where the files it names relative to it are.
	From string `yaml:"-"`
}

// ListenerConfig is one more address the HTTP side is served on.
type ListenerConfig struct {
	// Addr is where to listen: `:5443`.
	Addr string `yaml:"addr"`

	// Tls is the listener's handshake; none is plain HTTP. With
	// `client_ca_file`, a caller's certificate is verified against it, and
	// the policy's `mtls` provider vouches for whoever presented one. With
	// `client_cert_optional` too, a caller without one is still served, as
	// the anonymous caller.
	Tls config.TlsConfig `yaml:"tls"`
}
