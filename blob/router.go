package blob

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strings"

	"github.com/lesomnus/flob"
)

// Route places the repositories under Prefix on Stores. A prefix matches a
// name that is the prefix, or that continues it after a slash: `library`
// covers `library/ubuntu` and not `librarything`. The empty prefix covers
// everything.
type Route struct {
	Prefix string
	Stores flob.Stores
}

// Router is [flob.Stores] over several pools, choosing one per repository by
// the longest matching prefix: zot's sub-paths, for a registry whose
// namespaces want different disks or buckets.
//
// A mount between repositories on different pools is a copy; flob answers
// [flob.ErrIncompatibleStore] to the link and the registry reads and writes.
type Router struct {
	routes []Route
}

// NewRouter answers a router over routes. Without a route for the empty
// prefix, a name nothing matches has nowhere to go, so one is required.
func NewRouter(routes ...Route) (*Router, error) {
	rs := slices.Clone(routes)
	slices.SortStableFunc(rs, func(a, b Route) int { return len(b.Prefix) - len(a.Prefix) })
	if len(rs) == 0 || rs[len(rs)-1].Prefix != "" {
		return nil, errors.New("router: no route for the empty prefix")
	}
	for i := 1; i < len(rs); i++ {
		if rs[i].Prefix == rs[i-1].Prefix {
			return nil, errors.New("router: two routes for " + rs[i].Prefix)
		}
	}
	return &Router{routes: rs}, nil
}

func covers(prefix, name string) bool {
	if prefix == "" || name == prefix {
		return true
	}
	return strings.HasPrefix(name, prefix) && name[len(prefix)] == '/'
}

// Stores is the pool the repository named id is on.
func (r *Router) Stores(id string) flob.Stores {
	for _, rt := range r.routes {
		if covers(rt.Prefix, id) {
			return rt.Stores
		}
	}
	return nil
}

func (r *Router) Use(id string) flob.Store {
	return r.Stores(id).Use(id)
}

// Pools is every distinct pool, most specific route first.
func (r *Router) Pools() []flob.Stores {
	var out []flob.Stores
	for _, rt := range r.routes {
		if !slices.Contains(out, rt.Stores) {
			out = append(out, rt.Stores)
		}
	}
	return out
}

// PruneStages prunes every pool that can be pruned.
func (r *Router) PruneStages(ctx context.Context) (int, error) {
	n := 0
	var errs []error
	for _, p := range r.Pools() {
		c, ok := flob.AsStageCleaner(p)
		if !ok {
			continue
		}
		k, err := c.PruneStages(ctx)
		n += k
		if err != nil {
			errs = append(errs, err)
		}
	}
	return n, errors.Join(errs...)
}

// Namespaces is every namespace of every pool that the router would send to
// that pool: a namespace left on a pool by a route that was since changed is
// not the router's to list.
func (r *Router) Namespaces(ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		for _, p := range r.Pools() {
			n, ok := flob.AsNamespacer(p)
			if !ok {
				continue
			}
			for ns, err := range n.Namespaces(ctx) {
				if err != nil {
					if !yield("", err) {
						return
					}
					continue
				}
				if r.Stores(ns) != p {
					continue
				}
				if !yield(ns, nil) {
					return
				}
			}
		}
	}
}

var (
	_ flob.Stores       = (*Router)(nil)
	_ flob.StageCleaner = (*Router)(nil)
	_ flob.Namespacer   = (*Router)(nil)
)
