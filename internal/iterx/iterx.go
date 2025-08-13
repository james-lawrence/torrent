package iterx

import "iter"

func First[T any](i iter.Seq[T]) (T, bool) {
	next, stop := iter.Pull(i)
	defer stop()
	return next()
}
