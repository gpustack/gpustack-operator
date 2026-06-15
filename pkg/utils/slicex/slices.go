package slicex

import "gpustack.ai/gpustack/pkg/utils/typex"

// FilterTransform filters and maps the provided slice s using the function f.
// For each element in s, f is called with the element as an argument.
// If f returns true, the returned value is included in the resulting slice.
func FilterTransform[From, To any, S ~[]From](s S, f func(From) (To, bool)) []To {
	if len(s) == 0 {
		return nil
	}
	ret := make([]To, 0, len(s))
	for i := range s {
		r, ok := f(s[i])
		if !ok {
			continue
		}
		ret = append(ret, r)
	}
	if len(ret) == 0 {
		return nil
	}
	return ret
}

// Transform maps the provided slice s using the function f.
// For each element in s, f is called with the element as an argument,
// and the returned value is included in the resulting slice.
func Transform[From, To any, S ~[]From](s S, f func(From) To) []To {
	if len(s) == 0 {
		return nil
	}
	ret := make([]To, len(s))
	for i := range s {
		ret[i] = f(s[i])
	}
	if len(ret) == 0 {
		return nil
	}
	return ret
}

// FilterMap filters and maps the provided slice s to a map using the function f.
// For each element in s, f is called with the element as an argument.
// If f returns true, the returned key-value pair is included in the resulting map.
func FilterMap[K comparable, V any, S ~[]E, E any](s S, f func(E) (K, V, bool)) map[K]V {
	if len(s) == 0 {
		return nil
	}
	ret := make(map[K]V, len(s))
	for i := range s {
		k, v, ok := f(s[i])
		if !ok {
			continue
		}
		ret[k] = v
	}
	if len(ret) == 0 {
		return nil
	}
	return ret
}

// Map maps the provided slice s to a map using the function f.
// For each element in s, f is called with the element as an argument,
// and the returned key-value pair is included in the resulting map.
func Map[K comparable, V any, S ~[]E, E any](s S, f func(E) (K, V)) map[K]V {
	if len(s) == 0 {
		return nil
	}
	ret := make(map[K]V, len(s))
	for i := range s {
		k, v := f(s[i])
		ret[k] = v
	}
	if len(ret) == 0 {
		return nil
	}
	return ret
}

// Filter filters the provided slice s using the function f.
// For each element in s, f is called with the element as an argument.
// If f returns true, the element is included in the resulting slice.
func Filter[From any, S ~[]From](s S, f func(From) bool) []From {
	if len(s) == 0 {
		return s
	}
	ret := make([]From, 0, len(s))
	for i := range s {
		if f(s[i]) {
			ret = append(ret, s[i])
		}
	}
	return ret
}

// Sum sums the values obtained by applying the function f to each element in the slice s.
func Sum[T any, S ~[]T, I typex.Integer](s S, f func(T) I) I {
	var sum I
	for i := range s {
		sum += f(s[i])
	}
	return sum
}

// All checks if all elements in the slice s satisfy the condition defined by the function f.
func All[T any, S ~[]T](s S, f func(int) bool) bool {
	for i := range s {
		if !f(i) {
			return false
		}
	}
	return true
}

// Any checks if any element in the slice s satisfies the condition defined by the function f.
func Any[T any, S ~[]T](s S, f func(int) bool) bool {
	for i := range s {
		if f(i) {
			return true
		}
	}
	return false
}
