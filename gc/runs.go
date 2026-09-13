package gc

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"
)

const (
	KindOnline = "online"
	KindFull   = "full"

	TriggerSchedule = "schedule"
	TriggerAdmin    = "admin"
	TriggerCli      = "cli"

	StateRunning = "running"
	StateDone    = "done"
	StateFailed  = "failed"
)

var (
	ErrRunNotFound = errors.New("no such run")

	// ErrRunning is a full collection asked for while this process runs one.
	ErrRunning = errors.New("a full collection is already running")
)

// Run is one collection and what it did.
type Run struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Trigger string `json:"trigger"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`

	Stages       int      `json:"stages"`
	Tags         int      `json:"tags"`
	Manifests    int      `json:"manifests"`
	Repositories int      `json:"repositories"`
	Blobs        int      `json:"blobs"`
	Bytes        int64    `json:"bytes"`
	Missing      []string `json:"missing"`

	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished"`
}

func (r *Run) finish(rep FullReport, err error, at time.Time) {
	r.Stages, r.Tags, r.Manifests = rep.Stages, rep.Tags, rep.Manifests
	r.Repositories, r.Blobs, r.Bytes = rep.Repositories, rep.Blobs, rep.Bytes
	r.Missing = rep.Missing
	if r.Missing == nil {
		r.Missing = []string{}
	}
	r.State = StateDone
	if err != nil {
		r.State = StateFailed
		r.Error = err.Error()
	}
	r.Finished = &at
}

// Runs is where runs are recorded.
type Runs interface {
	Start(ctx context.Context, kind, trigger string) (Run, error)
	Finish(ctx context.Context, run Run) error
	Get(ctx context.Context, id string) (Run, error)

	// List is the most recent runs, newest first.
	List(ctx context.Context, n int) ([]Run, error)
}

// MemRuns keeps runs in memory, for tests and for a process with no database.
type MemRuns struct {
	mu   sync.Mutex
	runs []Run
}

func (m *MemRuns) Start(ctx context.Context, kind, trigger string) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := Run{ID: strconv.Itoa(len(m.runs) + 1), Kind: kind, Trigger: trigger, State: StateRunning, Missing: []string{}, Started: time.Now().UTC()}
	m.runs = append(m.runs, r)
	return r, nil
}

func (m *MemRuns) Finish(ctx context.Context, run Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.runs {
		if m.runs[i].ID == run.ID {
			m.runs[i] = run
			return nil
		}
	}
	return ErrRunNotFound
}

func (m *MemRuns) Get(ctx context.Context, id string) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return Run{}, ErrRunNotFound
}

func (m *MemRuns) List(ctx context.Context, n int) ([]Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := slices.Clone(m.runs)
	slices.Reverse(out)
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}
