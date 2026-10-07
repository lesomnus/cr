package cmd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lesomnus/cr/cmd"
)

// TestSecretIsWithoutTheWhitespaceAroundIt is cr's rule for a credential, which
// is looser than cfg's: only the space around the value goes, from a file or
// as it is written.
func TestSecretIsWithoutTheWhitespaceAroundIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pw")
	require.NoError(t, os.WriteFile(path, []byte(" hunter2 \r\n"), 0o600))

	for given, want := range map[string]string{
		"${file:" + path + "}": "hunter2",
		"  dckr_pat_x\t":       "dckr_pat_x",
		"$not{file:a}":         "$not{file:a}",
	} {
		var s cmd.Secret
		require.NoError(t, s.UnmarshalText([]byte(given)))
		v, err := s.Value()
		require.NoError(t, err)
		require.Equal(t, want, v, given)
	}

	// Nothing but whitespace is no credential.
	var s cmd.Secret
	require.Error(t, s.UnmarshalText([]byte(" \n")))
}

// TestSecretDoesNotSeeAnInPlaceRewrite is what is NOT noticed, held to on
// purpose: a writer that rewrites the file in place, same length, inside one
// tick. Nothing about it moved. That is the contract saying `rename` -- which
// is how a credential is published -- rather than a reason to read the file on
// every request.
func TestSecretDoesNotSeeAnInPlaceRewrite(t *testing.T) {
	x := require.New(t)

	path := filepath.Join(t.TempDir(), "token")
	x.NoError(os.WriteFile(path, []byte("first"), 0o600))

	var s cmd.Secret
	x.NoError(s.UnmarshalText([]byte("${file:" + path + "}")))
	got, err := s.Value()
	x.NoError(err)
	x.Equal("first", got)
	was, err := os.Stat(path)
	x.NoError(err)

	f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	x.NoError(err)
	_, err = f.WriteString("secnd")
	x.NoError(err)
	x.NoError(f.Close())
	x.NoError(os.Chtimes(path, was.ModTime(), was.ModTime()))

	got, err = s.Value()
	x.NoError(err)
	x.Equal("first", got)
}
