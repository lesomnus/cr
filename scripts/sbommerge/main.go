// Command sbommerge writes one CycloneDX document out of two: the bill of
// materials made at build time and the one made of the image once pushed.
//
//	sbommerge <build.cdx.json> <image.cdx.json>
//
// Each sees what the other cannot. The build's is made from the binary beside
// the module cache (`Dockerfile`, the `notices` stage), so it knows every
// module's license; it knows nothing of the base the binary is copied onto.
// The image's is made by scanning what was pushed, so it sees the base's
// packages -- whatever the base is, without this repository knowing where a
// distroless image keeps its package list -- and the modules again, but not
// their licenses, which are not in the image. Together they are the image and
// what its parts are licensed under, and that is what is attached to it as a
// referrer.
//
// A component both name is written once. Two are the same when their purls
// are, less the qualifiers (`?arch=…`, `?type=module`), which the two scans do
// not agree on; a component with no purl -- a file, the operating system -- is
// known by its type and name. Of the two, the one that carries more licenses
// is kept, and on a tie the build's.
//
// The rest of the document is the image's: its `metadata.component` is the
// image, which is what the document now describes. It gets a serial number of
// its own, since it is not either document it was made from, and its
// dependency graph is dropped: the components it would name are now partly the
// build's, whose `bom-ref`s the graph does not use.
package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: sbommerge <build.cdx.json> <image.cdx.json>")
		os.Exit(2)
	}
	if err := run(os.Stdout, os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "sbommerge:", err)
		os.Exit(1)
	}
}

func run(w io.Writer, buildPath, imagePath string) error {
	build, err := read(buildPath)
	if err != nil {
		return err
	}
	image, err := read(imagePath)
	if err != nil {
		return err
	}

	out, err := merge(build, image, serial())
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func read(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if doc["bomFormat"] != "CycloneDX" {
		return nil, fmt.Errorf("%s is not a CycloneDX document", path)
	}
	return doc, nil
}

// merge is [run] without the files: the image's document, with the components
// of both, written once each.
func merge(build, image map[string]any, serialNumber string) (map[string]any, error) {
	var (
		order []string
		kept  = map[string]map[string]any{}
	)
	for _, doc := range []map[string]any{build, image} {
		comps, err := components(doc)
		if err != nil {
			return nil, err
		}
		for _, c := range comps {
			k := key(c)
			prev, ok := kept[k]
			if !ok {
				order = append(order, k)
				kept[k] = c
				continue
			}
			if licenses(c) > licenses(prev) {
				kept[k] = c
			}
		}
	}

	out := make(map[string]any, len(image))
	for k, v := range image {
		out[k] = v
	}
	merged := make([]any, 0, len(order))
	for _, k := range order {
		merged = append(merged, kept[k])
	}
	out["components"] = merged
	out["serialNumber"] = serialNumber
	out["version"] = 1
	delete(out, "dependencies")
	return out, nil
}

func components(doc map[string]any) ([]map[string]any, error) {
	raw, _ := doc["components"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for i, v := range raw {
		c, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("component %d is not an object", i)
		}
		out = append(out, c)
	}
	return out, nil
}

// key is what makes two components the same one.
func key(c map[string]any) string {
	if p, _ := c["purl"].(string); p != "" {
		if i := strings.IndexByte(p, '?'); i >= 0 {
			p = p[:i]
		}
		return p
	}
	t, _ := c["type"].(string)
	n, _ := c["name"].(string)
	return "nopurl:" + t + ":" + n
}

func licenses(c map[string]any) int {
	l, _ := c["licenses"].([]any)
	return len(l)
}

// serial is a fresh `urn:uuid:` (version 4) for the merged document.
func serial() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
