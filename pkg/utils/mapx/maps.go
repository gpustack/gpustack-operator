package mapx

// Contains returns true if a contains all the keys in b with the same value.
func Contains[K, V comparable, A, B ~map[K]V](a A, b B) bool {
	for k, bv := range b {
		if av, found := a[k]; !found || av != bv {
			return false
		}
	}
	return true
}

// FilterTransform filters and maps the provided map s using the function f.
// For each key-value pair in s, f is called with the key and value as arguments.
// If f returns true, the returned key-value pair is included in the resulting map.
func FilterTransform[K comparable, V any, S ~[]K](s S, f func(K) (K, V, bool)) map[K]V {
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

// Transform maps the provided map s using the function f.
// For each key-value pair in s, f is called with the key and value as arguments,
// and the returned key-value pair is included in the resulting map.
func Transform[K comparable, V any, S ~[]K](s S, f func(K) (K, V)) map[K]V {
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

// FilterSlice filters and maps the provided map m to a slice using the function f.
// For each key-value pair in m, f is called with the key and value as arguments.
// If f returns true, the returned value is included in the resulting slice.
func FilterSlice[K comparable, V, Y any](m map[K]V, f func(K, V) (Y, bool)) []Y {
	if len(m) == 0 {
		return nil
	}
	ret := make([]Y, 0, len(m))
	for k, v := range m {
		if y, ok := f(k, v); ok {
			ret = append(ret, y)
		}
	}
	if len(ret) == 0 {
		return nil
	}
	return ret
}

// Slice maps the provided map m to a slice using the function f.
// For each key-value pair in m, f is called with the key and value as arguments,
// and the returned value is included in the resulting slice.
func Slice[K comparable, V, Y any](m map[K]V, f func(K, V) Y) []Y {
	if len(m) == 0 {
		return nil
	}
	ret := make([]Y, 0, len(m))
	for k, v := range m {
		ret = append(ret, f(k, v))
	}
	if len(ret) == 0 {
		return nil
	}
	return ret
}

// Filter filters the provided map m using the function f.
// For each key-value pair in m, f is called with the key and value as arguments.
func Filter[K comparable, V any](m map[K]V, f func(K, V) bool) map[K]V {
	if len(m) == 0 {
		return nil
	}
	ret := make(map[K]V, len(m))
	for k, v := range m {
		if f(k, v) {
			ret[k] = v
		}
	}
	if len(ret) == 0 {
		return nil
	}
	return ret
}

// EqualWithKeys reports whether two maps contain the same key/value pairs for the specified keys.
func EqualWithKeys[K, V comparable](a, b map[K]V, k K, ks ...K) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	av, aok := a[k]
	bv, bok := b[k]
	if !aok || !bok || av != bv {
		return false
	}
	for i := range ks {
		av, aok = a[ks[i]]
		bv, bok = b[ks[i]]
		if !aok || !bok || av != bv {
			return false
		}
	}
	return true
}
