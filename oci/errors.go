package oci

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// Code is an error code of the spec's envelope.
type Code string

const (
	CodeBlobUnknown         Code = "BLOB_UNKNOWN"
	CodeBlobUploadInvalid   Code = "BLOB_UPLOAD_INVALID"
	CodeBlobUploadUnknown   Code = "BLOB_UPLOAD_UNKNOWN"
	CodeDigestInvalid       Code = "DIGEST_INVALID"
	CodeManifestBlobUnknown Code = "MANIFEST_BLOB_UNKNOWN"
	CodeManifestInvalid     Code = "MANIFEST_INVALID"
	CodeManifestUnknown     Code = "MANIFEST_UNKNOWN"
	CodeNameInvalid         Code = "NAME_INVALID"
	CodeNameUnknown         Code = "NAME_UNKNOWN"
	CodeSizeInvalid         Code = "SIZE_INVALID"
	CodeUnauthorized        Code = "UNAUTHORIZED"
	CodeDenied              Code = "DENIED"
	CodeUnsupported         Code = "UNSUPPORTED"
	CodeTooManyRequests     Code = "TOOMANYREQUESTS"

	// CodeUnknown is not in the spec's table. It is what distribution answers
	// a failure with that is nobody's fault but the registry's, and clients
	// know it.
	CodeUnknown Code = "UNKNOWN"
)

var messages = map[Code]string{
	CodeBlobUnknown:         "blob unknown to registry",
	CodeBlobUploadInvalid:   "blob upload invalid",
	CodeBlobUploadUnknown:   "blob upload unknown to registry",
	CodeDigestInvalid:       "provided digest did not match uploaded content",
	CodeManifestBlobUnknown: "manifest references a manifest or blob unknown to registry",
	CodeManifestInvalid:     "manifest invalid",
	CodeManifestUnknown:     "manifest unknown to registry",
	CodeNameInvalid:         "invalid repository name",
	CodeNameUnknown:         "repository name not known to registry",
	CodeSizeInvalid:         "provided length did not match content length",
	CodeUnauthorized:        "authentication required",
	CodeDenied:              "requested access to the resource is denied",
	CodeUnsupported:         "the operation is unsupported",
	CodeTooManyRequests:     "too many requests",
	CodeUnknown:             "unknown error",
}

// Error is one entry of the envelope, with the status it is answered with.
type Error struct {
	Status  int    `json:"-"`
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`

	// Header is set on the response beside the envelope: `Range` on a 416,
	// `WWW-Authenticate` on a 401, `Retry-After` on a 503.
	Header http.Header `json:"-"`
}

func (e *Error) Error() string {
	if e.Detail != nil {
		if s, ok := e.Detail.(string); ok {
			return string(e.Code) + ": " + e.Message + ": " + s
		}
	}
	return string(e.Code) + ": " + e.Message
}

// NewError is an error with the spec's message for code.
func NewError(status int, code Code, detail any) *Error {
	return &Error{Status: status, Code: code, Message: messages[code], Detail: detail}
}

// WithHeader answers a copy of e that also sets k to v.
func (e *Error) WithHeader(k, v string) *Error {
	c := *e
	c.Header = c.Header.Clone()
	if c.Header == nil {
		c.Header = http.Header{}
	}
	c.Header.Set(k, v)
	return &c
}

func ErrBlobUnknown(detail any) *Error {
	return NewError(http.StatusNotFound, CodeBlobUnknown, detail)
}

func ErrBlobUploadUnknown(detail any) *Error {
	return NewError(http.StatusNotFound, CodeBlobUploadUnknown, detail)
}

func ErrBlobUploadInvalid(detail any) *Error {
	return NewError(http.StatusBadRequest, CodeBlobUploadInvalid, detail)
}

func ErrDigestInvalid(detail any) *Error {
	return NewError(http.StatusBadRequest, CodeDigestInvalid, detail)
}

func ErrManifestBlobUnknown(detail any) *Error {
	return NewError(http.StatusBadRequest, CodeManifestBlobUnknown, detail)
}

func ErrManifestInvalid(detail any) *Error {
	return NewError(http.StatusBadRequest, CodeManifestInvalid, detail)
}

func ErrManifestUnknown(detail any) *Error {
	return NewError(http.StatusNotFound, CodeManifestUnknown, detail)
}

func ErrNameInvalid(detail any) *Error {
	return NewError(http.StatusBadRequest, CodeNameInvalid, detail)
}

func ErrNameUnknown(detail any) *Error {
	return NewError(http.StatusNotFound, CodeNameUnknown, detail)
}

func ErrSizeInvalid(detail any) *Error {
	return NewError(http.StatusBadRequest, CodeSizeInvalid, detail)
}

func ErrUnauthorized(detail any) *Error {
	return NewError(http.StatusUnauthorized, CodeUnauthorized, detail)
}

func ErrDenied(detail any) *Error {
	return NewError(http.StatusForbidden, CodeDenied, detail)
}

func ErrUnsupported(detail any) *Error {
	return NewError(http.StatusMethodNotAllowed, CodeUnsupported, detail)
}

func ErrTooManyRequests(detail any) *Error {
	return NewError(http.StatusTooManyRequests, CodeTooManyRequests, detail)
}

// ErrUnavailable is a write that could not get its turn in time. The spec has
// no code for it; clients retry a 503 with `Retry-After`.
func ErrUnavailable(retryAfterSeconds int, detail any) *Error {
	return NewError(http.StatusServiceUnavailable, CodeTooManyRequests, detail).
		WithHeader("Retry-After", strconv.Itoa(retryAfterSeconds))
}

type envelope struct {
	Errors []*Error `json:"errors"`
}

// WriteError answers err as the envelope. An error that is not an [*Error] is
// the registry's own failure and is answered as 500 UNKNOWN, with nothing of
// it leaked to the client.
func WriteError(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		e = NewError(http.StatusInternalServerError, CodeUnknown, nil)
	}
	h := w.Header()
	for k, vs := range e.Header {
		h[k] = vs
	}
	h.Set("Content-Type", "application/json")
	h.Del("Content-Length")
	h.Del("Docker-Content-Digest")
	w.WriteHeader(e.Status)
	json.NewEncoder(w).Encode(envelope{Errors: []*Error{e}})
}
