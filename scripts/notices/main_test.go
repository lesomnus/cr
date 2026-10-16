package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEscapeIsTheModuleCacheEncoding(t *testing.T) {
	if got := escape("github.com/Azure/go-ntlmssp"); got != "github.com/!azure/go-ntlmssp" {
		t.Errorf("escape = %q", got)
	}
}

func TestNoticesComeFromTheModuleCacheForWhatTheSBOMNames(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "mod")
	goroot := filepath.Join(dir, "go")
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(cache, "github.com/!azure/thing@v1.0.0/LICENSE"), "MIT text, Copyright Azure\n")
	write(filepath.Join(cache, "github.com/!azure/thing@v1.0.0/NOTICE.md"), "a notice\n")
	write(filepath.Join(cache, "github.com/!azure/thing@v1.0.0/README.md"), "not a notice\n")
	write(filepath.Join(goroot, "LICENSE"), "Go's BSD text\n")
	if err := os.MkdirAll(filepath.Join(cache, "example.com/bare@v0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}

	sbom := filepath.Join(dir, "sbom.cdx.json")
	write(sbom, `{"bomFormat":"CycloneDX","components":[
		{"name":"github.com/Azure/thing","version":"v1.0.0","purl":"pkg:golang/github.com/Azure/thing@v1.0.0",
		 "licenses":[{"license":{"id":"MIT"}}]},
		{"name":"stdlib","version":"go1.27.1","purl":"pkg:golang/stdlib@1.27.1",
		 "licenses":[{"license":{"id":"BSD-3-Clause"}}]},
		{"name":"example.com/bare","version":"v0.1.0","purl":"pkg:golang/example.com/bare@v0.1.0"},
		{"name":"github.com/lesomnus/cr","version":"(devel)","purl":"pkg:golang/github.com/lesomnus/cr@(devel)"}
	]}`)

	var out bytes.Buffer
	missing, err := run(&out, sbom, cache, goroot, "github.com/lesomnus/cr", "")
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()

	for _, want := range []string{
		"github.com/Azure/thing v1.0.0", "License: MIT", "Copyright Azure", "a notice", "stdlib 1.27.1", "Go's BSD text",
		"Source: https://proxy.golang.org/github.com/!azure/thing/@v/v1.0.0.zip",
		"Source: https://go.dev/dl/go1.27.1.src.tar.gz",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the notices lack %q", want)
		}
	}
	if strings.Contains(got, "not a notice") {
		t.Error("a README was taken for a notice")
	}
	if strings.Contains(got, "lesomnus/cr") {
		t.Error("the image's own module is in its third-party notices")
	}
	if strings.Index(got, "stdlib") < strings.Index(got, "example.com/bare") {
		t.Error("the toolchain is not last")
	}
	if len(missing) != 1 || missing[0] != "example.com/bare@v0.1.0" {
		t.Errorf("missing = %v, want the module with no license file", missing)
	}
}

func TestAPrivateModuleHasNoPublicSource(t *testing.T) {
	for _, tc := range []struct {
		private, path string
		public        bool
	}{
		{"", "github.com/a/b", true},
		{"hday.io", "hday.io", false},
		{"hday.io", "hday.io/tpm", false},
		{"hday.io", "hday.iox/tpm", true},
		{"*.corp.example,github.com/acme", "git.corp.example/x", false},
		{"*.corp.example,github.com/acme", "github.com/acme/y", false},
		{"*.corp.example,github.com/acme", "github.com/other/y", true},
	} {
		got := source(module{Path: tc.path, Version: "v1.0.0"}, tc.private) != ""
		if got != tc.public {
			t.Errorf("GOPRIVATE=%q %s: public source = %v, want %v", tc.private, tc.path, got, tc.public)
		}
	}
}
