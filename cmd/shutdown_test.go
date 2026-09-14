package cmd

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// serving is a server's two listeners on loopback, with h on the HTTP one.
func serving(t *testing.T, h http.Handler) (*Server, *grpc.Server, *http.Server, string) {
	t.Helper()

	g := grpc.NewServer()
	gl, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go g.Serve(gl)

	srv := httpServer(context.Background(), h)
	hl, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go srv.Serve(hl)

	t.Cleanup(func() {
		srv.Close()
		g.Stop()
	})
	return &Server{}, g, srv, hl.Addr().String()
}

// get is a request on a connection of its own, so that one kept alive from an
// earlier request cannot answer for a listener that has closed.
func get(addr, path string) (string, error) {
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	res, err := c.Get("http://" + addr + path)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return string(b), err
}

func TestShutdownDrainsThenLetsRequestsFinish(t *testing.T) {
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(500 * time.Millisecond)
		io.WriteString(w, "done")
	})
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "pong")
	})
	s, g, srv, addr := serving(t, mux)

	slow := make(chan string, 1)
	go func() {
		v, err := get(addr, "/slow")
		if err != nil {
			v = err.Error()
		}
		slow <- v
	}()
	<-started

	require.NoError(t, s.Stopping(context.Background()))
	begun := time.Now()
	stopped := make(chan struct{})
	go func() {
		s.shutdown(context.Background(), ShutdownConfig{Drain: 300 * time.Millisecond, Timeout: 5 * time.Second}, g, srv)
		close(stopped)
	}()

	// Told to stop, it is not ready at once, and still answers through the
	// drain.
	require.Eventually(t, func() bool { return s.Stopping(context.Background()) != nil }, time.Second, 5*time.Millisecond)
	v, err := get(addr, "/ping")
	require.NoError(t, err)
	require.Equal(t, "pong", v)

	// What was in flight finishes, and only then is the server stopped.
	require.Equal(t, "done", <-slow)
	<-stopped
	require.GreaterOrEqual(t, time.Since(begun), 300*time.Millisecond)

	_, err = get(addr, "/ping")
	require.Error(t, err, "the listener is closed")
}

func TestShutdownEndsConnectStreams(t *testing.T) {
	started := make(chan struct{})
	ended := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/app.RepositoryService/Watch", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(ended)
	})
	s, g, srv, addr := serving(t, mux)

	go func() {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/app.RepositoryService/Watch", strings.NewReader("\x00\x00\x00\x00\x02{}"))
		req.Header.Set("Content-Type", "application/connect+json")
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
	}()
	<-started

	begun := time.Now()
	s.shutdown(context.Background(), ShutdownConfig{Timeout: 5 * time.Second}, g, srv)
	<-ended
	require.Less(t, time.Since(begun), 2*time.Second, "a stream does not hold the stop until the timeout")
}

func TestShutdownCutsAtTheTimeout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	mux := http.NewServeMux()
	mux.HandleFunc("/stuck", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	})
	s, g, srv, addr := serving(t, mux)

	go get(addr, "/stuck")
	<-started

	begun := time.Now()
	s.shutdown(context.Background(), ShutdownConfig{Timeout: 200 * time.Millisecond}, g, srv)
	elapsed := time.Since(begun)
	require.GreaterOrEqual(t, elapsed, 200*time.Millisecond)
	require.Less(t, elapsed, 2*time.Second)
}
