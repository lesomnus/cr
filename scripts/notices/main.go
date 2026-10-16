// Command notices writes the third-party notices of a Go binary, from the bill
// of materials syft made of it.
//
//	notices <sbom.cdx.json>
//
// The SBOM says which modules are linked into the binary and under what
// license; it does not carry what most of those licenses ask to be passed on,
// which is their text and the copyright line above it. Those are in the module
// cache, beside the source the binary was built from, so this reads them out of
// there for every module the SBOM names -- the one list, so the two documents
// cannot disagree about what is in the image.
//
// Each module is followed by where its source is, at exactly that version: the
// module proxy's zip of it, or the toolchain's source archive for the standard
// library. MPL-2.0 asks that whoever receives the binary be told where the
// source of its files can be had, and the line costs nothing for the rest. A
// module GOPRIVATE names is not on the public proxy, and gets no line.
//
// A module that ships no license file is not an error here: it is listed at the
// end, by name, so that it is seen rather than silently missing, and the exit
// status stays 0. Pass -strict to make it one.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

func main() {
	strict := flag.Bool("strict", false, "fail when a module ships no license file")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: notices [-strict] <sbom.cdx.json>")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	missing, err := run(os.Stdout, flag.Arg(0), modCache(), goRoot(), mainModule(), os.Getenv("GOPRIVATE"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "notices: no license file in %d module(s): %s\n", len(missing), strings.Join(missing, ", "))
		if *strict {
			os.Exit(1)
		}
	}
}

// The parts of a CycloneDX document this reads.
type document struct {
	Components []component `json:"components"`
}

type component struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	PURL     string `json:"purl"`
	Licenses []struct {
		License *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"license"`
		Expression string `json:"expression"`
	} `json:"licenses"`
}

// module is one Go module the binary links, as its purl names it.
type module struct {
	Path    string
	Version string
	License string
}

func run(w io.Writer, sbomPath, cache, goroot, main, private string) ([]string, error) {
	raw, err := os.ReadFile(sbomPath)
	if err != nil {
		return nil, err
	}
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("read %s: %w", sbomPath, err)
	}

	mods := modules(doc, main)
	fmt.Fprintf(w, "Third-party software in this image\n\n")
	fmt.Fprintf(w, "This image contains a Go binary built with the modules below. Each is\n")
	fmt.Fprintf(w, "listed with its version, its license as detected (SPDX), where its source\n")
	fmt.Fprintf(w, "at that version can be obtained, and the license and notice files it\n")
	fmt.Fprintf(w, "ships, reproduced as they are.\n")

	var missing []string
	for _, m := range mods {
		dir := filepath.Join(cache, escape(m.Path)+"@"+escape(m.Version))
		if m.Path == "stdlib" {
			dir = goroot
		}
		files, err := noticeFiles(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s@%s: %w", m.Path, m.Version, err)
		}

		fmt.Fprintf(w, "\n%s\n%s %s\n", strings.Repeat("=", 78), m.Path, m.Version)
		if m.License != "" {
			fmt.Fprintf(w, "License: %s\n", m.License)
		}
		if src := source(m, private); src != "" {
			fmt.Fprintf(w, "Source: %s\n", src)
		}
		if len(files) == 0 {
			fmt.Fprintf(w, "(no license file in the module)\n")
			missing = append(missing, m.Path+"@"+m.Version)
			continue
		}
		for _, f := range files {
			body, err := os.ReadFile(filepath.Join(dir, f))
			if err != nil {
				return nil, fmt.Errorf("%s@%s: %w", m.Path, m.Version, err)
			}
			fmt.Fprintf(w, "%s\n%s:\n\n%s", strings.Repeat("-", 78), f, body)
			if len(body) > 0 && body[len(body)-1] != '\n' {
				fmt.Fprintln(w)
			}
		}
	}
	return missing, nil
}

// modules lists the Go modules the SBOM names, sorted, the main module left
// out: it is the image's own, and its license goes beside this file, not in it.
// It is named rather than recognised by its version, which is `(devel)` in a
// build context with no `.git/` and a pseudo-version in one with.
func modules(doc document, main string) []module {
	seen := map[string]bool{}
	var out []module
	for _, c := range doc.Components {
		path, version, ok := golangPURL(c.PURL)
		if !ok || path == main || version == "(devel)" {
			continue
		}
		key := path + "@" + version
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, module{Path: path, Version: version, License: license(c)})
	}
	slices.SortFunc(out, func(a, b module) int {
		if a.Path == "stdlib" || b.Path == "stdlib" {
			// The toolchain last: it is the one every Go binary has.
			return boolCmp(a.Path == "stdlib", b.Path == "stdlib")
		}
		return strings.Compare(a.Path, b.Path)
	})
	return out
}

func boolCmp(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	default:
		return -1
	}
}

// golangPURL reads `pkg:golang/<path>@<version>`.
func golangPURL(purl string) (string, string, bool) {
	if !strings.HasPrefix(purl, "pkg:golang/") {
		return "", "", false
	}
	s := strings.TrimPrefix(purl, "pkg:golang/")
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return "", "", false
	}
	path, err := url.PathUnescape(s[:at])
	if err != nil {
		return "", "", false
	}
	version, err := url.PathUnescape(s[at+1:])
	if err != nil {
		return "", "", false
	}
	return path, version, true
}

// license joins what the SBOM detected, as an SPDX expression.
func license(c component) string {
	var ids []string
	for _, l := range c.Licenses {
		switch {
		case l.Expression != "":
			ids = append(ids, l.Expression)
		case l.License != nil && l.License.ID != "":
			ids = append(ids, l.License.ID)
		case l.License != nil && l.License.Name != "":
			ids = append(ids, l.License.Name)
		}
	}
	slices.Sort(ids)
	return strings.Join(slices.Compact(ids), " AND ")
}

// noticeFiles names the license and notice files at the top of a module.
func noticeFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := strings.ToUpper(e.Name())
		for _, prefix := range []string{"LICENSE", "LICENCE", "COPYING", "COPYRIGHT", "NOTICE", "PATENTS", "UNLICENSE"} {
			if strings.HasPrefix(n, prefix) {
				out = append(out, e.Name())
				break
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// source is where the source of m at its version can be downloaded, or "" for a
// module the public proxy does not serve.
func source(m module, private string) string {
	if m.Path == "stdlib" {
		return "https://go.dev/dl/go" + strings.TrimPrefix(m.Version, "go") + ".src.tar.gz"
	}
	if matchesPrefixPattern(private, m.Path) {
		return ""
	}
	return "https://proxy.golang.org/" + escape(m.Path) + "/@v/" + escape(m.Version) + ".zip"
}

// matchesPrefixPattern reports whether a comma-separated list of glob patterns,
// as GOPRIVATE is written, matches path or a prefix of it, element by element
// (golang.org/x/mod/module.MatchPrefixPatterns).
func matchesPrefixPattern(globs, path string) bool {
	for _, glob := range strings.Split(globs, ",") {
		glob = strings.TrimSpace(glob)
		if glob == "" {
			continue
		}
		n := strings.Count(glob, "/") + 1
		elems := strings.Split(path, "/")
		if len(elems) < n {
			continue
		}
		if ok, _ := filepath.Match(glob, strings.Join(elems[:n], "/")); ok {
			return true
		}
	}
	return false
}

// escape is the module cache's case encoding: an upper-case letter is `!` and
// its lower case, so that two paths differing only in case do not collide on a
// file system that folds it (golang.org/x/mod/module.EscapePath).
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsUpper(r) {
			b.WriteByte('!')
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func modCache() string {
	if v := os.Getenv("GOMODCACHE"); v != "" {
		return v
	}
	if v := os.Getenv("GOPATH"); v != "" {
		return filepath.Join(strings.Split(v, string(os.PathListSeparator))[0], "pkg", "mod")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "go", "pkg", "mod")
}

// mainModule is the module in the working directory, which is the one built.
func mainModule() string {
	out, err := exec.Command("go", "list", "-m").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func goRoot() string {
	if v := os.Getenv("GOROOT"); v != "" {
		return v
	}
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
