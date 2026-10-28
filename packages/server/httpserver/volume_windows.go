//go:build windows

package httpserver

// volumeCapacity is not available on Windows. The status reports zeros.
func volumeCapacity(path string) (CapacityUsage, bool) {
	return CapacityUsage{}, false
}
