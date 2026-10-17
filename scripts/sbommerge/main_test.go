package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const buildDoc = `{"bomFormat":"CycloneDX","specVersion":"1.6","serialNumber":"urn:uuid:build",
 "metadata":{"component":{"type":"application","name":"cr"}},
 "components":[
  {"bom-ref":"b1","type":"library","name":"x/a","version":"v1","purl":"pkg:golang/x/a@v1","licenses":[{"license":{"id":"MIT"}}]},
  {"bom-ref":"b2","type":"library","name":"x/b","version":"v2","purl":"pkg:golang/x/b@v2"},
  {"bom-ref":"b3","type":"file","name":"/scan/cr"}]}`

const imageDoc = `{"bomFormat":"CycloneDX","specVersion":"1.6","serialNumber":"urn:uuid:image",
 "metadata":{"component":{"type":"container","name":"ghcr.io/lesomnus/cr"}},
 "components":[
  {"bom-ref":"i1","type":"library","name":"x/a","version":"v1","purl":"pkg:golang/x/a@v1?type=module"},
  {"bom-ref":"i2","type":"library","name":"x/b","version":"v2","purl":"pkg:golang/x/b@v2?type=module","licenses":[{"license":{"id":"BSD-3-Clause"}}]},
  {"bom-ref":"i3","type":"library","name":"tzdata","purl":"pkg:deb/debian/tzdata@2026b?distro=debian-12","licenses":[{"license":{"name":"public-domain"}}]},
  {"bom-ref":"i4","type":"operating-system","name":"debian"}],
 "dependencies":[{"ref":"i1","dependsOn":[]}]}`

func mergeOf(t *testing.T) map[string]any {
	t.Helper()
	var build, image map[string]any
	if err := json.Unmarshal([]byte(buildDoc), &build); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(imageDoc), &image); err != nil {
		t.Fatal(err)
	}
	out, err := merge(build, image, "urn:uuid:merged")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func refs(doc map[string]any) []string {
	var out []string
	for _, c := range doc["components"].([]any) {
		out = append(out, c.(map[string]any)["bom-ref"].(string))
	}
	slices.Sort(out)
	return out
}

func TestAComponentBothNameIsWrittenOnceWithTheLicensesEitherHas(t *testing.T) {
	// x/a: the build's has the license. x/b: the image's does, which happens when
	// the build's scan found none and the image's did.
	got := refs(mergeOf(t))
	want := []string{"b1", "b3", "i2", "i3", "i4"}
	if !slices.Equal(got, want) {
		t.Errorf("components = %v, want %v", got, want)
	}
}

func TestOnATieTheBuildsIsKept(t *testing.T) {
	var build, image map[string]any
	json.Unmarshal([]byte(`{"bomFormat":"CycloneDX","components":[{"bom-ref":"b","purl":"pkg:golang/x@v1"}]}`), &build)             //nolint:errcheck
	json.Unmarshal([]byte(`{"bomFormat":"CycloneDX","components":[{"bom-ref":"i","purl":"pkg:golang/x@v1?type=module"}]}`), &image) //nolint:errcheck
	out, err := merge(build, image, "urn:uuid:x")
	if err != nil {
		t.Fatal(err)
	}
	if got := refs(out); !slices.Equal(got, []string{"b"}) {
		t.Errorf("components = %v, want the build's", got)
	}
}

func TestTheDocumentIsTheImagesWithASerialOfItsOwnAndNoGraph(t *testing.T) {
	out := mergeOf(t)
	if name := out["metadata"].(map[string]any)["component"].(map[string]any)["name"]; name != "ghcr.io/lesomnus/cr" {
		t.Errorf("metadata.component = %v, want the image", name)
	}
	if out["serialNumber"] != "urn:uuid:merged" {
		t.Errorf("serialNumber = %v", out["serialNumber"])
	}
	if _, ok := out["dependencies"]; ok {
		t.Error("the dependency graph was kept; it names bom-refs the merge may have replaced")
	}
}

func TestRunReadsFilesAndRefusesWhatIsNotCycloneDX(t *testing.T) {
	dir := t.TempDir()
	b := filepath.Join(dir, "build.cdx.json")
	i := filepath.Join(dir, "image.cdx.json")
	os.WriteFile(b, []byte(buildDoc), 0o644) //nolint:errcheck
	os.WriteFile(i, []byte(imageDoc), 0o644) //nolint:errcheck

	var w bytes.Buffer
	if err := run(&w, b, i); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Bytes(), &out); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if s, _ := out["serialNumber"].(string); !strings.HasPrefix(s, "urn:uuid:") || s == "urn:uuid:image" {
		t.Errorf("serialNumber = %q, want a fresh urn:uuid", s)
	}

	spdx := filepath.Join(dir, "spdx.json")
	os.WriteFile(spdx, []byte(`{"spdxVersion":"SPDX-2.3"}`), 0o644) //nolint:errcheck
	if err := run(&w, spdx, i); err == nil {
		t.Error("an SPDX document was merged as if it were CycloneDX")
	}
}
