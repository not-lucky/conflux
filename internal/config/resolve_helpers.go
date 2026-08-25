package config

import "time"

// dur parses and assigns a duration field. If v is nil it is a no-op (the
// caller's existing default is kept). On a parse error it returns a
// field-wrapped error carrying the same sentinel as parseDuration
// (ErrBadDuration or ErrDurationNotPositive) with the exact message
// "invalid duration <value>".
func dur(out *time.Duration, field string, v *string) error {
	if v == nil {
		return nil
	}
	d, err := parseDuration(*v)
	if err != nil {
		return wrap(err, field, "invalid duration "+*v)
	}
	*out = d
	return nil
}

// intval validates and assigns an integer field with a lower bound. If v is
// nil it is a no-op. If *v is below min it returns a field-wrapped error
// carrying errSentinel with the exact message "must be >=<min>, got <value>".
func intval(out *int, field string, v *int, min int, errSentinel error) error {
	if v == nil {
		return nil
	}
	if *v < min {
		return wrapf(errSentinel, field, "must be >=%d, got %d", min, *v)
	}
	*out = *v
	return nil
}

// boolval assigns a bool field. If v is nil it is a no-op.
func boolval(out *bool, v *bool) {
	if v != nil {
		*out = *v
	}
}
