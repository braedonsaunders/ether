// Package paging applies bounded offset/limit windows without integer overflow.
package paging

// Bounds returns a safe half-open slice interval. A negative offset is treated
// as zero; a negative limit means no limit, while zero requests an empty page.
func Bounds(total, offset, limit int) (start, end int) {
	if total < 0 {
		total = 0
	}
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	start = offset
	if limit < 0 || limit > total-start {
		return start, total
	}
	return start, start + limit
}

// Slice returns the requested page, preserving the source order.
func Slice[T any](items []T, offset, limit int) []T {
	start, end := Bounds(len(items), offset, limit)
	return items[start:end]
}
