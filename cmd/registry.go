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

	Gc GcConfig `yaml:"gc"`

	// Proxies make the repositories under a prefix a pull-through cache of
	// another registry.
	Proxies []ProxyConfig `yaml:"proxies"`
}

// ProxyConfig is one pull-through cache.
type ProxyConfig struct {
	// Prefix is the repositories that are the cache: `docker.io` covers
	// `docker.io/library/ubuntu`. Empty covers every repository, which makes
	// the deployment a mirror and nothing else.
	Prefix string `yaml:"prefix"`

	// Upstream is the registry's URL: `https://registry-1.docker.io`.
	Upstream string `yaml:"upstream"`

	// Remote is the upstream repository the prefix stands for; empty maps
	// what follows the prefix to itself.
	Remote string `yaml:"remote"`

	Username string `yaml:"username"`
	Password string `yaml:"password"`

	// TagTtl is how long a tag is served from the cache before the upstream
	// is asked again; zero is five minutes.
	TagTtl time.Duration `yaml:"tag_ttl"`

	// Retention is how long the cache keeps what nobody pulls; zero keeps
	// it until a full collection finds it unreferenced.
	Retention time.Duration `yaml:"retention"`
}

// StorageConfig says where blobs and manifests are kept.
type StorageConfig struct {
	// Driver is `os`, `s3` or `memory`.
	Driver string `yaml:"driver"`

	Os OsStorageConfig `yaml:"os"`
	S3 S3StorageConfig `yaml:"s3"`

	Upload   UploadConfig   `yaml:"upload"`
	Redirect RedirectConfig `yaml:"redirect"`

	// Routes put the repositories under a prefix on another store; the one
	// above holds everything no route covers. The longest prefix wins, and a
	// prefix covers the names that continue it after a slash.
	Routes []StorageRouteConfig `yaml:"routes"`
}

type StorageRouteConfig struct {
	Prefix string `yaml:"prefix"`

	Driver string          `yaml:"driver"`
	Os     OsStorageConfig `yaml:"os"`
	S3     S3StorageConfig `yaml:"s3"`
}

type OsStorageConfig struct {
	// Root is the directory flob keeps everything under.
	Root string `yaml:"root"`
}

// S3StorageConfig is a bucket on S3 or anything that speaks it with
// conditional writes: MinIO, and not every other.
type S3StorageConfig struct {
	// Endpoint is the service's URL; empty is AWS in Region.
	Endpoint string `yaml:"endpoint"`

	// PublicEndpoint is the URL clients reach for a redirect, when it is not
	// the one cr reaches: a CDN, a public hostname.
	PublicEndpoint string `yaml:"public_endpoint"`

	Region string `yaml:"region"`
	Bucket string `yaml:"bucket"`

	// Prefix is where in the bucket this deployment keeps everything.
	Prefix string `yaml:"prefix"`

	AccessKeyId     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	SessionToken    string `yaml:"session_token"`

	// PathStyle addresses the bucket in the path rather than the host, which
	// MinIO and most S3-compatible servers want.
	PathStyle bool `yaml:"path_style"`

	// PartSize is how much of an upload is buffered per part; zero is 16 MiB.
	PartSize int64 `yaml:"part_size"`
}

// UploadConfig is how long a chunked upload lives.
type UploadConfig struct {
	// TTL is how long an upload nobody appends to survives; zero is 24 hours.
	TTL time.Duration `yaml:"ttl"`

	// Retention is how long a finished upload's receipt is kept, so a retried
	// final PUT gets the same answer; zero is 24 hours.
	Retention time.Duration `yaml:"retention"`
}

// RedirectConfig is whether a blob GET is sent to the object store.
type RedirectConfig struct {
	// Enabled answers a blob GET with a 307 to a presigned URL when the
	// store can sign one. Only S3 can.
	Enabled bool `yaml:"enabled"`

	// Ttl is how long a presigned URL is good for; zero is 15 minutes.
	Ttl time.Duration `yaml:"ttl"`
}

// GcConfig is the collection that runs on its own: expired uploads, untagged
// manifests, and retention rules.
type GcConfig struct {
	// Every is how often it runs; zero is an hour, and a negative duration
	// never runs it.
	Every time.Duration `yaml:"every"`

	// Untagged is how old a manifest nothing tags, holds or refers to must be
	// before it is deleted; zero keeps them.
	Untagged time.Duration `yaml:"untagged"`

	// Delay is how long a blob must have been in a repository before a full
	// collection's sweep erases it, so that a push in flight keeps its blobs;
	// zero is an hour, and a negative duration erases at once.
	Delay time.Duration `yaml:"delay"`

	// FullEvery is how often a full collection, which also sweeps every
	// repository's store, runs on its own; zero never. `POST /admin/gc` and
	// `cr gc --full` run one when asked.
	FullEvery time.Duration `yaml:"full_every"`
}
