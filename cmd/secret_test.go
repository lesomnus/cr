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
