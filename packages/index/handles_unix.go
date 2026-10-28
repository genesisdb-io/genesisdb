//go:build linux || darwin || freebsd || netbsd || openbsd

package index

import "syscall"

// shardHandleLimit returns wanted, or a quarter of the descriptor limit if
// that is smaller, so requests, event logs and readers keep enough room.
func shardHandleLimit(wanted int) int {
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return 64
	}
	if quarter := int(limit.Cur / 4); quarter < wanted {
		return max(quarter, 16)
	}
	return wanted
}
