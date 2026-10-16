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
// The binary is not all the image ships: the base is Debian's, and some of its
// packages are GPL-2.0. Given the base's dpkg database (-dpkg, a status.d
// directory as distroless keeps one, or a status file), the notices end with
// those packages, each with where its copyright is in the image and where the
// source of exactly that version is kept: snapshot.debian.org, which keeps
// every version Debian ever published.
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
	dpkg := flag.String("dpkg", "", "the base image's dpkg status file or status.d directory, to list its packages")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: notices [-strict] [-dpkg <status>] <sbom.cdx.json>")
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
	if *dpkg != "" {
		pkgs, err := readDpkg(*dpkg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "notices:", err)
			os.Exit(1)
		}
		writeDebian(os.Stdout, pkgs)
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

// debPackage is one installed Debian package, and the source it was built from.
type debPackage struct {
	Name          string
	Version       string
	Source        string
	SourceVersion string
}

// readDpkg reads a dpkg status file, or a status.d directory of one file per
// package beside its .md5sums.
func readDpkg(path string) ([]debPackage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	var files []string
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() && !strings.HasSuffix(e.Name(), ".md5sums") {
				files = append(files, filepath.Join(path, e.Name()))
			}
		}
	} else {
		files = []string{path}
	}

	var out []debPackage
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		for _, para := range strings.Split(string(raw), "\n\n") {
			if p, ok := parseStanza(para); ok {
				out = append(out, p)
			}
		}
	}
	slices.SortFunc(out, func(a, b debPackage) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// parseStanza reads the fields of one package. `Source:` is absent when the
// source package has the binary's name, and carries a version in parentheses
// when it differs from the binary's (a binNMU, or a package built from another).
func parseStanza(para string) (debPackage, bool) {
	fields := map[string]string{}
	for _, line := range strings.Split(para, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if ok {
			fields[k] = strings.TrimSpace(v)
		}
	}
	p := debPackage{Name: fields["Package"], Version: fields["Version"]}
	if p.Name == "" || p.Version == "" {
		return debPackage{}, false
	}
	if st := fields["Status"]; st != "" && !strings.HasSuffix(st, " installed") {
		return debPackage{}, false
	}
	p.Source, p.SourceVersion = p.Name, p.Version
	if src := fields["Source"]; src != "" {
		name, ver, _ := strings.Cut(src, " ")
		p.Source = name
		if ver = strings.Trim(ver, "()"); ver != "" {
			p.SourceVersion = ver
		}
	}
	return p, true
}

func writeDebian(w io.Writer, pkgs []debPackage) {
	if len(pkgs) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\nDebian packages of the base image\n\n", strings.Repeat("=", 78))
	fmt.Fprintf(w, "The image is built on Debian. Each package's copyright and license terms\n")
	fmt.Fprintf(w, "are in /usr/share/doc/<package>/copyright in this image, and its source, at\n")
	fmt.Fprintf(w, "the version here, is at the address beside it.\n\n")
	for _, p := range pkgs {
		fmt.Fprintf(w, "%s %s\n", p.Name, p.Version)
		fmt.Fprintf(w, "  Copyright: /usr/share/doc/%s/copyright\n", p.Name)
		fmt.Fprintf(w, "  Source: https://snapshot.debian.org/package/%s/%s/\n", url.PathEscape(p.Source), url.PathEscape(p.SourceVersion))
	}
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
