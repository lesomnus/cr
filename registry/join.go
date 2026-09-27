package registry

import (
	"context"
	"sync"
	"time"
)

// joinTimeout bounds a fetch that requests join. It does not end with the
// request that started it, since the others are waiting on it too, so this
// is what ends one whose upstream stopped answering halfway.
const joinTimeout = 2 * time.Minute

// flights are the fetches in progress, by what they fetch, so that requests
// for the same thing at the same time make one fetch between them.
type flights struct {
	mu sync.Mutex
	m  map[string]*flight
}

type flight struct {
	done    chan struct{}
	outcome string
	err     error
}

// join runs fill for key, or, when a fill for key is already running, waits
// for that one instead, and answers what it did and whether it was another
// request's. Either way a request stops waiting when its own context ends,
// and the fill goes on for whoever else is waiting and for the cache.
func (fs *flights) join(ctx context.Context, key string, fill func(context.Context) (string, error)) (string, error, bool) {
	fs.mu.Lock()
	f, joined := fs.m[key]
	if !joined {
		if fs.m == nil {
			fs.m = map[string]*flight{}
		}
		f = &flight{done: make(chan struct{})}
		fs.m[key] = f
		go func() {
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), joinTimeout)
			defer cancel()
			f.outcome, f.err = fill(fctx)

			fs.mu.Lock()
			delete(fs.m, key)
			fs.mu.Unlock()
			close(f.done)
		}()
	}
	fs.mu.Unlock()

	select {
	case <-f.done:
		return f.outcome, f.err, joined
	case <-ctx.Done():
		return "error", ctx.Err(), joined
	}
}
