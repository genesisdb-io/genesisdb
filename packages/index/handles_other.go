//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package index

// shardHandleLimit returns wanted, platforms without rlimit have no small
// descriptor limit.
func shardHandleLimit(wanted int) int { return wanted }
