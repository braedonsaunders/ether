//go:build !linux && !darwin && !windows

package hoststat

func (r *Resources) hostSample() (Sample, bool) {
	return Sample{}, false
}
