package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func mib(v int64) string {
	if v == 0 {
		return "–"
	}
	return fmt.Sprintf("%.1f", float64(v)/(1<<20))
}

// report reads the JSON lines `pull` wrote and writes them as a table, a row
// a run, in the order they ran.
func report(args []string) error {
	var in io.Reader = os.Stdin
	if len(args) > 0 {
		f, err := os.Open(args[0])
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}

	var b strings.Builder
	b.WriteString("| registry | scenario | clients | ok | wall s | slowest client s | manifest max s | blob TTFB median / max s | RssAnon peak MiB | cgroup peak MiB | anon / file / sock peak MiB | limit hit | OOM kills | registry exited |\n")
	b.WriteString("| --- | --- | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |\n")
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var notes []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Result
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return fmt.Errorf("report: %w", err)
		}
		slowest := 0.0
		if n := len(r.Client); n > 0 {
			slowest = r.Client[n-1]
		}
		s := r.Stats
		if s == nil {
			s = &Stats{}
		}
		peak := s.CgroupPeak
		if peak == 0 {
			peak = s.CgroupCurrentPeak
		}
		ok := "yes"
		if !r.OK {
			ok = "**no**"
			for _, e := range r.Errors {
				notes = append(notes, fmt.Sprintf("- %s, %s, %d: %s", r.Label, r.Scenario, r.Clients, e))
			}
		}
		exited := s.Exited
		switch {
		case r.SamplerGone:
			exited = "container ended"
		case exited == "":
			exited = "–"
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %s | %.1f | %.1f | %.2f | %.2f / %.2f | %s | %s | %s / %s / %s | %d | %d | %s |\n",
			r.Label, r.Scenario, r.Clients, ok, r.Wall, slowest, r.ManifestMax, r.TTFBMed, r.TTFBMax,
			mib(s.RssAnonPeak), mib(peak), mib(s.CgroupAnonPeak), mib(s.CgroupFilePeak), mib(s.CgroupSockPeak),
			s.Events["max"], s.Events["oom_kill"], exited)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(notes) > 0 {
		b.WriteString("\nErrors:\n\n" + strings.Join(notes, "\n") + "\n")
	}
	_, err := io.WriteString(os.Stdout, b.String())
	return err
}
