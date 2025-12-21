package fnvx

import (
	"cmp"
	"hash/fnv"
)

func Uint32[T ~[]byte | ~string](s T) uint32 {
	g := fnv.New32a()
	g.Write([]byte(s))
	return g.Sum32()
}

func Cmp[T ~[]byte | ~string](a, b T) int {
	return cmp.Compare(Uint32(a), Uint32(b))
}
