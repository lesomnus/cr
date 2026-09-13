package cmd

import (
	"time"
)

// RegistryConfig is what the distribution API is served with.
type RegistryConfig struct {
	Storage StorageConfig `yaml:"storage"`

	// MaxManifestSize is the largest manifest a push may carry, in bytes;
	// zero is 4 MiB.
	MaxManifestSize int64 `yaml:"max_manifest_size"`

	// DisableWellKnown sends the constant blobs -- `{}`, the empty layer, the
	// empty tar, the empty blob -- to the store like any other, rather than
	// answering them from memory.
	DisableWellKnown bool `yaml:"disable_well_known"`

	// LockWait bounds how long a write that changes what a repository
	// references waits for that repository's lock before it is answered 503;
	// zero is thirty seconds.
	LockWait time.Duration `yaml:"lock_wait"`
}

// StorageConfig says where blobs and manifests are kept.
type StorageConfig struct {
	// Driver is `os` or `memory`.
	Driver string `yaml:"driver"`

	Os OsStorageConfig `yaml:"os"`

	Upload UploadConfig `yaml:"upload"`
}

type OsStorageConfig struct {
	// Root is the directory flob keeps everything under.
	Root string `yaml:"root"`
}

// UploadConfig is how long a chunked upload lives.
type UploadConfig struct {
	// TTL is how long an upload nobody appends to survives; zero is 24 hours.
	TTL time.Duration `yaml:"ttl"`

	// Retention is how long a finished upload's receipt is kept, so a retried
	// final PUT gets the same answer; zero is 24 hours.
	Retention time.Duration `yaml:"retention"`
}
