package errorsx

// String useful wrapper for turning string constants
// into errors that interopt well with stdlib functionality.
type String string

func (t String) Error() string {
	return string(t)
}
