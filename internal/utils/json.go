package utils

func OrEmpty[T any](slice []T) any {
	if len(slice) == 0 {
		return []T{}
	}
	return slice
}
