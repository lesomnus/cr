package blob

import (
	"slices"

	"github.com/lesomnus/flob"
)

// CacheRoute makes the repositories under Prefix a pull-through cache of
// Origin.
type CacheRoute struct {
	Prefix string
	Origin flob.Stores
}

// Cache is [flob.Stores] where the repositories a route covers read through
// flob's cache -- a miss streams from the origin while the store fills, and a
// herd of misses shares one fill -- and every other repository is base as it
// is.
type Cache struct {
	base   flob.Stores
	routes []cacheRoute
}

type cacheRoute struct {
	prefix string
	stores *flob.CacheStores
}

func NewCache(base flob.Stores, routes ...CacheRoute) *Cache {
	c := &Cache{base: base}
	for _, r := range routes {
		c.routes = append(c.routes, cacheRoute{prefix: r.Prefix, stores: flob.NewCacheStores(base, r.Origin)})
	}
	slices.SortStableFunc(c.routes, func(a, b cacheRoute) int { return len(b.prefix) - len(a.prefix) })
	return c
}

func (c *Cache) Use(id string) flob.Store {
	for _, r := range c.routes {
		if Covers(r.prefix, id) {
			return r.stores.Use(id)
		}
	}
	return c.base.Use(id)
}

// Unwrap is the store under every cache, which is what walks, lists
// namespaces and prunes uploads.
func (c *Cache) Unwrap() flob.Stores { return c.base }

// Covers reports whether a route for prefix covers the repository name: the
// prefix itself, or a name that continues it after a slash. The empty prefix
// covers everything.
func Covers(prefix, name string) bool {
	return covers(prefix, name)
}
