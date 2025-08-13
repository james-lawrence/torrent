package stmutil

import (
	"iter"
	"unsafe"

	"github.com/benbjohnson/immutable"
)

// This is the type constraint for keys passed through from github.com/benbjohnson/immutable.
type KeyConstraint interface {
	comparable
}

type Iterable[T any] interface {
	Iter() iter.Seq[T]
}

type Settish[K KeyConstraint] interface {
	Iterable[K]
	Add(K) Settish[K]
	Delete(K) Settish[K]
	Contains(K) bool
	Range(func(K) bool)
	Len() int
}

type mapToSet[K KeyConstraint] struct {
	m Mappish[K, struct{}]
}

type interhash[K KeyConstraint] struct{}

func (interhash[K]) Hash(x K) uint32 {
	ptr := uintptr(unsafe.Pointer(&x))
	return immutable.NewHasher(ptr).Hash(ptr)
}

func (interhash[K]) Equal(i, j K) bool {
	return i == j
}

func NewSet[K KeyConstraint]() Settish[K] {
	return mapToSet[K]{NewMap[K, struct{}]()}
}

func NewSortedSet[K KeyConstraint](lesser lessFunc[K]) Settish[K] {
	return mapToSet[K]{NewSortedMap[K, struct{}](lesser)}
}

func (s mapToSet[K]) Add(x K) Settish[K] {
	s.m = s.m.Set(x, struct{}{})
	return s
}

func (s mapToSet[K]) Delete(x K) Settish[K] {
	s.m = s.m.Delete(x)
	return s
}

func (s mapToSet[K]) Len() int {
	return s.m.Len()
}

func (s mapToSet[K]) Contains(x K) bool {
	_, ok := s.m.Get(x)
	return ok
}

func (s mapToSet[K]) Range(f func(K) bool) {
	s.m.Range(func(k K, _ struct{}) bool {
		return f(k)
	})
}

func (s mapToSet[K]) Iter() iter.Seq[K] {
	return func(yield func(K) bool) {
		s.Range(func(k K) bool {
			return yield(k)
		})
	}
}

type Map[K KeyConstraint, V any] struct {
	*immutable.Map[K, V]
}

func NewMap[K KeyConstraint, V any]() Mappish[K, V] {
	return Map[K, V]{immutable.NewMap[K, V](interhash[K]{})}
}

func (m Map[K, V]) Delete(x K) Mappish[K, V] {
	m.Map = m.Map.Delete(x)
	return m
}

func (m Map[K, V]) Set(key K, value V) Mappish[K, V] {
	m.Map = m.Map.Set(key, value)
	return m
}

func (sm Map[K, V]) Range(f func(K, V) bool) {
	iter := sm.Map.Iterator()
	for {
		k, v, ok := iter.Next()
		if !ok {
			break
		}
		if !f(k, v) {
			return
		}
	}
}

func (sm Map[K, V]) Iter() iter.Seq[K] {
	return func(yield func(K) bool) {
		sm.Range(func(key K, _ V) bool {
			return yield(key)
		})
	}
}

type SortedMap[K KeyConstraint, V any] struct {
	*immutable.SortedMap[K, V]
}

func (sm SortedMap[K, V]) Set(key K, value V) Mappish[K, V] {
	sm.SortedMap = sm.SortedMap.Set(key, value)
	return sm
}

func (sm SortedMap[K, V]) Delete(key K) Mappish[K, V] {
	sm.SortedMap = sm.SortedMap.Delete(key)
	return sm
}

func (sm SortedMap[K, V]) Range(f func(key K, value V) bool) {
	iter := sm.SortedMap.Iterator()
	for {
		k, v, ok := iter.Next()
		if !ok {
			break
		}
		if !f(k, v) {
			return
		}
	}
}

func (sm SortedMap[K, V]) Iter() iter.Seq[K] {
	return func(yield func(K) bool) {
		sm.Range(func(key K, _ V) bool {
			return yield(key)
		})
	}
}

type lessFunc[T KeyConstraint] func(l, r T) bool

type comparer[K KeyConstraint] struct {
	less lessFunc[K]
}

func (me comparer[K]) Compare(i, j K) int {
	if me.less(i, j) {
		return -1
	} else if me.less(j, i) {
		return 1
	} else {
		return 0
	}
}

func NewSortedMap[K KeyConstraint, V any](less lessFunc[K]) Mappish[K, V] {
	return SortedMap[K, V]{
		SortedMap: immutable.NewSortedMap[K, V](comparer[K]{less}),
	}
}

type Mappish[K, V any] interface {
	Set(K, V) Mappish[K, V]
	Delete(key K) Mappish[K, V]
	Get(key K) (V, bool)
	Range(func(K, V) bool)
	Len() int
	Iter() iter.Seq[K]
}

func GetLeft(l, _ any) any {
	return l
}

type Lenner interface {
	Len() int
}
