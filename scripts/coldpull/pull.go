package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
)

const manifestAccept = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

// Result is one run: N clients pulling at once through one registry.
type Result struct {
	Label    string `json:"label"`
	Scenario string `json:"scenario"`
	Clients  int    `json:"clients"`
	Parallel int    `json:"parallel"`

	OK     bool     `json:"ok"`
	Errors []string `json:"errors,omitempty"`

	Wall    float64   `json:"wall_s"`   // first request to last byte
	Client  []float64 `json:"client_s"` // each client's pull, in the order they finished
	Bytes   int64     `json:"bytes"`    // read by every client together
	TTFBMax float64   `json:"ttfb_max_s"`
	TTFBMed float64   `json:"ttfb_median_s"`

	// How long a client waited for the image's manifest: a registry that
	// copies an image before it answers for it shows here and not in TTFB.
	ManifestMax float64 `json:"manifest_max_s"`

	Stats *Stats `json:"stats,omitempty"`

	// SamplerGone is a container that ended with its sampler: an OOM kill
	// that took the registry and then, with the limit still exceeded, the
	// only process left. Stats are then as last read during the pulls.
	SamplerGone bool `json:"sampler_gone,omitempty"`
}

type descriptor struct {
	MediaType string            `json:"mediaType"`
	Digest    digest.Digest     `json:"digest"`
	Size      int64             `json:"size"`
	Platform  map[string]string `json:"platform"`
}

type manifest struct {
	MediaType string       `json:"mediaType"`
	Config    descriptor   `json:"config"`
	Layers    []descriptor `json:"layers"`
	Manifests []descriptor `json:"manifests"`
}

type puller struct {
	base     string
	c        *http.Client
	parallel int

	mu        sync.Mutex
	ttfb      []float64
	manifests []float64
	bytes     int64
}

func (p *puller) get(ctx context.Context, path, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+path, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	res, err := p.c.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		res.Body.Close()
		return nil, fmt.Errorf("GET %s: %s %s", path, res.Status, bytes.TrimSpace(b))
	}
	return res, nil
}

func (p *puller) manifest(ctx context.Context, repo, ref string) (manifest, error) {
	res, err := p.get(ctx, "/v2/"+repo+"/manifests/"+ref, manifestAccept)
	if err != nil {
		return manifest{}, err
	}
	defer res.Body.Close()
	var m manifest
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		return manifest{}, fmt.Errorf("manifest %s:%s: %w", repo, ref, err)
	}
	// An index: the platform a linux/amd64 docker would take.
	if len(m.Manifests) > 0 {
		for _, d := range m.Manifests {
			if d.Platform["os"] == "linux" && d.Platform["architecture"] == "amd64" {
				return p.manifest(ctx, repo, d.Digest.String())
			}
		}
		return manifest{}, fmt.Errorf("%s:%s: an index with no linux/amd64", repo, ref)
	}
	return m, nil
}

// blob reads d whole, checks it, and records how long its first byte took.
func (p *puller) blob(ctx context.Context, repo string, d digest.Digest) error {
	start := time.Now()
	res, err := p.get(ctx, "/v2/"+repo+"/blobs/"+d.String(), "")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	v := d.Algorithm().Digester()
	first := make([]byte, 1)
	n, err := io.ReadFull(res.Body, first)
	ttfb := time.Since(start).Seconds()
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return fmt.Errorf("blob %s: %w", d, err)
	}
	v.Hash().Write(first[:n])
	rest, err := io.Copy(v.Hash(), res.Body)
	if err != nil {
		return fmt.Errorf("blob %s: %w", d, err)
	}
	if v.Digest() != d {
		return fmt.Errorf("blob %s: read %s", d, v.Digest())
	}
	p.mu.Lock()
	p.ttfb = append(p.ttfb, ttfb)
	p.bytes += int64(n) + rest
	p.mu.Unlock()
	return nil
}

// image pulls one image the way docker does: the manifest, then the config
// and the layers, parallel at a time.
func (p *puller) image(ctx context.Context, ref string) error {
	repo, tag, ok := strings.Cut(ref, ":")
	if !ok {
		tag = "latest"
	}
	start := time.Now()
	m, err := p.manifest(ctx, repo, tag)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.manifests = append(p.manifests, time.Since(start).Seconds())
	p.mu.Unlock()
	ds := append([]descriptor{m.Config}, m.Layers...)
	sem := make(chan struct{}, p.parallel)
	errs := make(chan error, len(ds))
	var wg sync.WaitGroup
	for _, d := range ds {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			errs <- p.blob(ctx, repo, d.Digest)
		}()
	}
	wg.Wait()
	close(errs)
	var all []error
	for err := range errs {
		if err != nil {
			all = append(all, err)
		}
	}
	return errors.Join(all...)
}

func readStats(c *http.Client, url string) (*Stats, error) {
	res, err := c.Get(url + "/stats")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var s Stats
	if err := json.NewDecoder(res.Body).Decode(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func post(c *http.Client, url string) error {
	res, err := c.Post(url, "", nil)
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

func pull(args []string) error {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	registry := fs.String("registry", "http://registry:5000", "the registry under test")
	statsURL := fs.String("stats", "http://registry:9100", "its `coldpull run`; empty reads no stats")
	clients := fs.Int("clients", 1, "clients pulling at once")
	scenario := fs.String("scenario", "same", "`same`: every client pulls the first image; `distinct`: client i pulls image i")
	images := fs.String("images", "", "comma-separated images to pull; empty is coldpull/img0 … as `coldpull upstream` serves them")
	count := fs.Int("image-count", 8, "how many coldpull/imgN there are, when -images is empty")
	parallel := fs.Int("parallel", 3, "blobs one client reads at once; docker's default is 3")
	label := fs.String("label", "", "what this run is called in the report")
	wait := fs.Duration("wait", time.Minute, "how long to wait for the registry to answer /v2/")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long the pulls may take")
	fs.Parse(args)

	var refs []string
	if *images != "" {
		refs = strings.Split(*images, ",")
	} else {
		for i := range *count {
			refs = append(refs, imageName(i)+":latest")
		}
	}
	if *scenario != "same" && *scenario != "distinct" {
		return fmt.Errorf("pull: -scenario %q: want same or distinct", *scenario)
	}

	c := &http.Client{Transport: &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 64}}
	ctl := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(*wait)
	for {
		res, err := ctl.Get(*registry + "/v2/")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pull: %s did not answer /v2/ within %s", *registry, *wait)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if *statsURL != "" {
		if err := post(ctl, *statsURL+"/reset"); err != nil {
			return fmt.Errorf("pull: reset the stats: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	p := &puller{base: *registry, c: c, parallel: *parallel}
	r := Result{Label: *label, Scenario: *scenario, Clients: *clients, Parallel: *parallel}

	// The stats as the pulls go, so that a container that ends with its
	// sampler still leaves what it reached on the way.
	var (
		last     *Stats
		lastMu   sync.Mutex
		pollDone = make(chan struct{})
		polled   = make(chan struct{})
	)
	if *statsURL != "" {
		go func() {
			defer close(polled)
			t := time.NewTicker(250 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-pollDone:
					return
				case <-t.C:
					if s, err := readStats(ctl, *statsURL); err == nil {
						lastMu.Lock()
						last = s
						lastMu.Unlock()
					}
				}
			}
		}()
	} else {
		close(polled)
	}

	var (
		mu   sync.Mutex
		errs []string
		wg   sync.WaitGroup
	)
	start := time.Now()
	for i := range *clients {
		ref := refs[0]
		if *scenario == "distinct" {
			ref = refs[i%len(refs)]
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.Now()
			err := p.image(ctx, ref)
			mu.Lock()
			defer mu.Unlock()
			r.Client = append(r.Client, time.Since(t).Seconds())
			if err != nil {
				errs = append(errs, strings.ReplaceAll(err.Error(), "\n", "; "))
			}
		}()
	}
	wg.Wait()
	r.Wall = time.Since(start).Seconds()
	close(pollDone)
	<-polled
	r.Bytes = p.bytes
	slices.Sort(r.Client)
	if len(p.ttfb) > 0 {
		slices.Sort(p.ttfb)
		r.TTFBMax = p.ttfb[len(p.ttfb)-1]
		r.TTFBMed = p.ttfb[len(p.ttfb)/2]
	}
	if len(p.manifests) > 0 {
		r.ManifestMax = slices.Max(p.manifests)
	}
	slices.Sort(errs)
	r.Errors = slices.Compact(errs)
	if len(r.Errors) > 5 {
		r.Errors = append(r.Errors[:5], fmt.Sprintf("… and %d more", len(r.Errors)-5))
	}
	r.OK = len(errs) == 0

	if *statsURL != "" {
		// A registry the OOM killer took drops its connections before
		// `run` has reaped it; give it the moment, so the stats say so.
		time.Sleep(500 * time.Millisecond)
		s, err := readStats(ctl, *statsURL)
		if err != nil {
			r.OK = false
			r.SamplerGone = true
			r.Errors = append(r.Errors, "the registry's container ended: "+err.Error())
			s = last
		}
		r.Stats = s
	}
	return json.NewEncoder(os.Stdout).Encode(r)
}
