package cmd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lesomnus/payday/config"
	"github.com/stretchr/testify/require"
)

// selfSigned writes a self-signed certificate and its key under dir, the
// shape a robot pins: no CA, the certificate is the trust.
func selfSigned(t *testing.T, dir, name string, usage x509.ExtKeyUsage, ip string) (crt, key string, pair tls.Certificate) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{usage},
		BasicConstraintsValid: true,
	}
	if ip != "" {
		tmpl.IPAddresses = []net.IP{net.ParseIP(ip)}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	require.NoError(t, err)
	kb, err := x509.MarshalECPrivateKey(k)
	require.NoError(t, err)

	crt, key = filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	cp := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kp := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	require.NoError(t, os.WriteFile(crt, cp, 0o600))
	require.NoError(t, os.WriteFile(key, kp, 0o600))
	pair, err = tls.X509KeyPair(cp, kp)
	require.NoError(t, err)
	return crt, key, pair
}

// One server, a plain listener and one that asks for client certificates,
// and the handler told which certificate verified on each.
func TestListeners(t *testing.T) {
	dir := t.TempDir()
	srvCrt, srvKey, _ := selfSigned(t, dir, "cr", x509.ExtKeyUsageServerAuth, "127.0.0.1")
	engCrt, _, engine := selfSigned(t, dir, "engine-thorb", x509.ExtKeyUsageClientAuth, "")
	_, _, stranger := selfSigned(t, dir, "stranger", x509.ExtKeyUsageClientAuth, "")

	ls, err := listen(Config{Listeners: []ListenerConfig{
		{Addr: "127.0.0.1:0"},
		{Addr: "127.0.0.1:0", Tls: config.TlsConfig{
			CertFile: srvCrt, KeyFile: srvKey,
			ClientCAFile: engCrt, ClientCertOptional: true,
		}},
	}})
	require.NoError(t, err)
	require.Len(t, ls, 2)
	require.False(t, ls[0].tls)
	require.True(t, ls[1].tls)

	srv := httpServer(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.TLS == nil:
			io.WriteString(w, "plain")
		case len(r.TLS.VerifiedChains) == 0:
			io.WriteString(w, "tls")
		default:
			io.WriteString(w, "tls "+r.TLS.VerifiedChains[0][0].Subject.CommonName)
		}
	}))
	for _, l := range ls {
		go srv.Serve(l)
	}
	t.Cleanup(func() { srv.Close() })

	var getWith func(base string, cfg *tls.Config) (string, error)
	pool := x509.NewCertPool()
	b, err := os.ReadFile(srvCrt)
	require.NoError(t, err)
	pool.AppendCertsFromPEM(b)
	get := func(base string, certs ...tls.Certificate) (string, error) {
		t.Helper()
		return getWith(base, &tls.Config{RootCAs: pool, Certificates: certs})
	}
	getWith = func(base string, cfg *tls.Config) (string, error) {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		res, err := c.Get(base + "/v2/")
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		return string(body), err
	}
	plain, secure := "http://"+ls[0].Addr().String(), "https://"+ls[1].Addr().String()

	got, err := get(plain)
	require.NoError(t, err)
	require.Equal(t, "plain", got)

	got, err = get(secure)
	require.NoError(t, err)
	require.Equal(t, "tls", got, "a caller without a certificate is still served")

	got, err = get(secure, engine)
	require.NoError(t, err)
	require.Equal(t, "tls engine-thorb", got)

	// A client offers only a certificate the listener says it trusts, so one
	// it does not is not sent, and the caller is anonymous.
	got, err = get(secure, stranger)
	require.NoError(t, err)
	require.Equal(t, "tls", got)

	// Sent anyway, it is no connection.
	_, err = getWith(secure, &tls.Config{RootCAs: pool, GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		return &stranger, nil
	}})
	require.Error(t, err)
}

func TestListenersRefuse(t *testing.T) {
	_, err := listen(Config{Listeners: []ListenerConfig{{}}})
	require.ErrorContains(t, err, "listeners[0].addr: not set")

	_, err = listen(Config{Listeners: []ListenerConfig{{Addr: "127.0.0.1:0", Tls: config.TlsConfig{ClientCAFile: "/nowhere"}}}})
	require.ErrorContains(t, err, "listeners[0].tls: both cert_file and key_file")
}
