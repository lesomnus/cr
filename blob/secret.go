package blob

// Secret is a credential as it is NOW: asked for each time it is used, so one
// that lives in a file can change under a running process.
//
// # Why a file
//
// A credential gets REPLACED. A bearer minted on the host — a device
// certificate presented to an authority, on a schedule of its own — expires,
// and a password is rotated; either way something drops the new one where cr
// can see it. A value held since the start holds the old one until somebody
// restarts the process, and the restart would be whatever the credential was
// needed for. A file is also how Kubernetes hands a Secret to a process.
//
// So the file is the interface, and the only thing cr has to do is notice.
// That is `${file:/path}` in the configuration, a `cmd.Secret`: the file is
// re-read when it changes, and a read that fails while a rotation is half done
// keeps the value in hand. The rules are xli's `cfg.SecretOf`, which took them
// from here.
//
// What this package asks of a credential is only that it is asked each time.
type Secret interface {
	Value() (string, error)
}

// Literal is a credential that is what it is, and stays that.
type Literal string

func (l Literal) Value() (string, error) { return string(l), nil }
