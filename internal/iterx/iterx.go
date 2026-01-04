package iterx

import (
	"context"
	"iter"
)

func First[T any](i iter.Seq[T]) (T, bool) {
	next, stop := iter.Pull(i)
	defer stop()
	return next()
}

type Seq[T any] interface {
	Each(context.Context) iter.Seq[T]
	Err() error
}
