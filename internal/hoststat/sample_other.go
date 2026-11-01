//go:build !linux

package hoststat

// hostSample has no portable CPU/memory percentages off Linux.
func hostSample() (Sample, bool) {
	return Sample{}, false
}
