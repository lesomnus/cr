package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// cgroupDir is the cgroup this process is in, as a container sees its own.
const cgroupDir = "/sys/fs/cgroup"

// Stats is what `run` reports about the registry it runs: peaks since the
// last reset, sampled every interval, beside what the kernel counted.
//
// The cgroup figures are the container's, so they include this sampler's
// own few MiB; RssAnon is the registry's alone.
type Stats struct {
	RssAnonPeak int64 `json:"rss_anon_peak"` // the registry's allocations, bytes
	RssPeak     int64 `json:"rss_peak"`

	// The cgroup: memory.current as sampled, and memory.stat's anon, file
	// (page cache, of blobs written) and sock (socket buffers), each at its
	// own peak. Absent without a cgroup v2 to read.
	CgroupCurrentPeak int64 `json:"cgroup_current_peak,omitempty"`
	CgroupAnonPeak    int64 `json:"cgroup_anon_peak,omitempty"`
	CgroupFilePeak    int64 `json:"cgroup_file_peak,omitempty"`
	CgroupSockPeak    int64 `json:"cgroup_sock_peak,omitempty"`

	// memory.peak as the kernel keeps it, since the container started, and
	// memory.max.
	CgroupPeak int64 `json:"cgroup_peak,omitempty"`
	CgroupMax  int64 `json:"cgroup_max,omitempty"` // 0 is no limit

	// memory.events since the last reset: `max` is how often the limit was
	// hit and reclaimed against, `oom_kill` what the OOM killer took, and
	// `sock_throttled` how often socket buffers were shrunk for it, where
	// the kernel counts that.
	Events map[string]int64 `json:"events,omitempty"`

	// Exited is how the registry ended, when it has.
	Exited string `json:"exited,omitempty"`
}

type sampler struct {
	pid int

	mu     sync.Mutex
	s      Stats
	events map[string]int64 // at the last reset
}

func readInt(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v := strings.TrimSpace(string(b))
	if v == "max" {
		return 0, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil
}

// keyed reads a file of `key value` lines.
func keyed(path string) map[string]int64 {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			out[k] = n
		}
	}
	return out
}

// status reads fields of /proc/<pid>/status, in bytes.
func status(pid int) map[string]int64 {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		fs := strings.Fields(v)
		if !ok || len(fs) != 2 || fs[1] != "kB" {
			continue
		}
		if n, err := strconv.ParseInt(fs[0], 10, 64); err == nil {
			out[k] = n << 10
		}
	}
	return out
}

func peak(p *int64, v int64) {
	if v > *p {
		*p = v
	}
}

func (s *sampler) sample() {
	st := status(s.pid)
	cur, hasCgroup := readInt(cgroupDir + "/memory.current")
	var ms map[string]int64
	if hasCgroup {
		ms = keyed(cgroupDir + "/memory.stat")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	peak(&s.s.RssAnonPeak, st["RssAnon"])
	peak(&s.s.RssPeak, st["VmRSS"])
	if hasCgroup {
		peak(&s.s.CgroupCurrentPeak, cur)
		peak(&s.s.CgroupAnonPeak, ms["anon"])
		peak(&s.s.CgroupFilePeak, ms["file"])
		peak(&s.s.CgroupSockPeak, ms["sock"])
	}
}

func (s *sampler) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	exited := s.s.Exited
	s.s = Stats{Exited: exited}
	s.events = keyed(cgroupDir + "/memory.events")
}

func (s *sampler) stats() Stats {
	s.sample()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.s
	out.CgroupPeak, _ = readInt(cgroupDir + "/memory.peak")
	out.CgroupMax, _ = readInt(cgroupDir + "/memory.max")
	if now := keyed(cgroupDir + "/memory.events"); now != nil {
		out.Events = map[string]int64{}
		for k, v := range now {
			out.Events[k] = v - s.events[k]
		}
	}
	return out
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	listen := fs.String("listen", ":9100", "where the stats are served: GET /stats, POST /reset")
	every := fs.Duration("every", 50*time.Millisecond, "how often to sample")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return errors.New("run: nothing to run; coldpull run [flags] -- command args…")
	}

	cmd := exec.Command(fs.Arg(0), fs.Args()[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	// The registry is the one the OOM killer takes. Alone in a container it
	// would be; beside this sampler the kernel may pick either, and taking
	// the sampler takes the container and every number with it. Raising a
	// score needs no privilege, and the registry runs as it would.
	if err := os.WriteFile("/proc/"+strconv.Itoa(cmd.Process.Pid)+"/oom_score_adj", []byte("1000"), 0); err != nil {
		log.Printf("run: the registry may not be the one an OOM kill takes: %v", err)
	}
	s := &sampler{pid: cmd.Process.Pid}
	s.reset()

	// A signal is the registry's while it runs, and ends this once it has.
	done := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for v := range sig {
			select {
			case <-done:
				os.Exit(0)
			default:
				cmd.Process.Signal(v)
			}
		}
	}()

	go func() {
		t := time.NewTicker(*every)
		defer t.Stop()
		for range t.C {
			s.sample()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(s.stats())
	})
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, r *http.Request) {
		s.reset()
	})
	go func() {
		if err := http.ListenAndServe(*listen, mux); err != nil {
			log.Fatal(err)
		}
	}()

	// The registry ending, killed by the OOM killer or otherwise, is a
	// result: say so in the stats and stay up to be asked.
	err := cmd.Wait()
	s.mu.Lock()
	if err != nil {
		s.s.Exited = err.Error()
	} else {
		s.s.Exited = "exit status 0"
	}
	s.mu.Unlock()
	close(done)
	log.Printf("run: the registry ended: %v", err)
	select {}
}
