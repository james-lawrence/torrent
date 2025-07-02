package atomicx

import (
	"sync/atomic"

	"golang.org/x/exp/constraints"
)

func Pointer[T any](v T) (r *atomic.Pointer[T]) {
	r = &atomic.Pointer[T]{}
	r.Store(&v)
	return r
}

func Uint32[T constraints.Integer](n T) (r *atomic.Uint32) {
	r = &atomic.Uint32{}
	r.Store(uint32(n))
	return r
}

func Bool(n bool) (r *atomic.Bool) {
	r = &atomic.Bool{}
	r.Store(n)
	return r
}
