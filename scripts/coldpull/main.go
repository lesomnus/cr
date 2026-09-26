// Command coldpull measures what a pull-through cache costs while several cold
// pulls are in flight at once: the memory the process and its cgroup reach,
// what the kernel had to do about it, and how long the clients waited.
//
// It is four tools in one binary, which `scripts/coldpull.sh` puts in one
// image and runs in containers:
//
//	coldpull upstream   a registry of synthetic images, made up as they are
//	                    read and served at a bounded rate per connection
//	coldpull run -- …   the registry under test as a child, sampled
//	coldpull pull       N concurrent cold pulls, and the samples, as JSON
//	coldpull report     those JSON lines as a markdown table
//
// See `scripts/coldpull.sh` for how they fit together.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "upstream":
		err = upstream(os.Args[2:])
	case "run":
		err = run(os.Args[2:])
	case "pull":
		err = pull(os.Args[2:])
	case "report":
		err = report(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "coldpull:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: coldpull upstream|run|pull|report [flags]")
	os.Exit(2)
}
