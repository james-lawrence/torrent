// Package langx provides small utility functions to extend the standard golang language.
package langx

import "reflect"

// Autoptr converts a value into a pointer
func Autoptr[T any](a T) *T {
	return &a
}

// Autoderef safely converts a pointer to its value, uses the zero value for nil.
func Autoderef[T any](a *T) (zero T) {
	if a == nil {
		return zero
	}

	return *a
}

func DerefOrZero[T any](a *T) (zero T) {
	if a == nil {
		return zero
	}

	return *a
}

func FirstNonZero[T comparable](s ...T) T {
	var (
		x T
	)

	for _, v := range s {
		if v == x {
			continue
		}

		return v
	}

	return x
}

func DefaultIfZero[T comparable](fallback T, v T) T {
	var (
		x T
	)

	if v != x {
		return v
	}

	return fallback
}

func Must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}

	return v
}

func MustZero[T any](v T, err error) T {
	var (
		x T
	)
	if err != nil {
		return x
	}

	return v
}

func Clone[T any, Y ~func(*T)](v T, options ...Y) T {
	dup := v
	for _, opt := range options {
		opt(&dup)
	}

	return dup
}

func Compose[T any, Y ~func(*T)](options ...Y) Y {
	return func(v *T) {
		for _, opt := range options {
			opt(v)
		}
	}
}

// It uses reflection to safely check for nilable types (pointers, interfaces, slices, maps, etc.)
// and safely handles non-nilable types (int, string, struct, etc.).
func FirstNonNil[T any](s ...T) T {
	var zero T

	for _, v := range s {
		// Use reflection to safely check if the value is nil.
		if !isNil(v) {
			return v
		}
	}

	return zero
}

// isNil uses reflection to determine if a value is nil.
// If the underlying type is not a nilable type (e.g., int, bool), it returns false.
// If the underlying type is a nilable type (e.g., pointer, slice, map, interface, func, channel)
// it returns whether the value is currently nil.
func isNil(i any) bool {
	if i == nil {
		return true
	}

	v := reflect.ValueOf(i)
	k := v.Kind()

	// Check if the Kind is one that can hold a nil value.
	//
	switch k {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Chan, reflect.Func, reflect.Interface:
		// v.IsNil() panics if v's Kind is not one of the nilable types,
		// but the switch statement ensures we only call it on those kinds.
		return v.IsNil()
	default:
		// For non-nilable kinds (like Int, String, Struct), we return false.
		return false
	}
}
