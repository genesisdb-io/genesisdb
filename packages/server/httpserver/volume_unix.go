//go:build !windows

package httpserver

import "syscall"

// volumeCapacity returns the capacity of the volume that holds path.
func volumeCapacity(path string) (CapacityUsage, bool) {
	var volume syscall.Statfs_t
	if err := syscall.Statfs(path, &volume); err != nil {
		return CapacityUsage{}, false
	}
	blockSize := int64(volume.Bsize)
	return CapacityUsage{
		Max:       int64(volume.Blocks) * blockSize,
		Used:      int64(volume.Blocks-volume.Bfree) * blockSize,
		Available: int64(volume.Bavail) * blockSize,
	}, true
}
