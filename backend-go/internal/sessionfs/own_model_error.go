package sessionfs

import "errors"

// OwnModelError marks an error about one of a profile's own model files (phase 3c.2a): its
// file, its base or the tuning it sets cannot be used. Its text keeps the path and the reason,
// which the profile's own session and the server log show. Another profile may not read them:
// the sandbox hides profile-own from other profiles, so a caller from another profile is told
// only that the profile's selected model could not be loaded (app.otherProfileError). It wraps
// the error it marks, so a ConfigError or ClientError is still one.
type OwnModelError struct{ Err error }

func (e *OwnModelError) Error() string { return e.Err.Error() }
func (e *OwnModelError) Unwrap() error { return e.Err }

// IsOwnModelError reports whether err is, or wraps, an error about an own model file.
func IsOwnModelError(err error) bool {
	var own *OwnModelError
	return errors.As(err, &own)
}

// ownModelError marks err as an error about an own model file; nil stays nil and a marked
// error is not marked twice.
func ownModelError(err error) error {
	if err == nil || IsOwnModelError(err) {
		return err
	}
	return &OwnModelError{Err: err}
}

// keepOwnModelMark is wrapped, an error built from cause's text, marked when cause is: a
// ConfigError made with %v keeps no type, so the mark is carried over by hand.
func keepOwnModelMark(cause, wrapped error) error {
	if IsOwnModelError(cause) {
		return ownModelError(wrapped)
	}
	return wrapped
}
