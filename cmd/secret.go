package cmd

import (
	"errors"
	"strings"

	"github.com/lesomnus/xli/cfg"
)

// Secret is a credential in the configuration: the value as it is written,
// `${env:NAME}`, or `${file:/path}` -- the file's content, re-read when the
// file changes, which is what lets a rotated credential be used without a
// restart. See [cfg.SecretOf] for the rules a file is read by; they are the
// ones cr kept in `blob` before cfg did, which is where they came from.
//
// `cr config` prints the reference, or `<redacted>` for a value written as it
// is, and nothing prints what the file holds.
type Secret = cfg.SecretOf[string, TrimSpace]

// TrimSpace reads a credential without the whitespace around it, which is
// cr's rule and a looser one than cfg's own: a token written by `echo` ends in
// a newline, and one pasted into an editor may begin or end in anything else
// that is not part of it. A credential that is nothing but whitespace is no
// credential.
type TrimSpace struct{}

func (TrimSpace) Decode(b []byte) (string, error) {
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", errors.New("empty")
	}
	return v, nil
}
