package plugin

import "errors"

type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent marks an error as non-retryable (docs/design/07-plugins.md §7.1).
// A nil error stays nil; marking a permanent error again preserves it.
func Permanent(err error) error {
	if err == nil || IsPermanent(err) {
		return err
	}
	return &permanentError{err: err}
}

// IsPermanent reports whether err wraps a non-retryable error, including joined errors.
func IsPermanent(err error) bool {
	var permanent *permanentError
	return errors.As(err, &permanent)
}
