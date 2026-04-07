package funcx

// Ternary returns trueVal if cond is true, and falseVal otherwise.
func Ternary[T any](cond bool, trueVal, falseVal T) T {
	if cond {
		return trueVal
	}
	return falseVal
}

// TernaryFunc returns the result of trueFunc if condFunc returns true, and the result of falseFunc otherwise.
func TernaryFunc[T any](condFunc func() bool, trueFunc, falseFunc func() T) T {
	if condFunc() {
		return trueFunc()
	}
	return falseFunc()
}
