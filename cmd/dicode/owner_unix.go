//go:build unix

package main

import (
	"os"
	"syscall"
)

// ownedByCurrentUser reports whether fi's owner is the calling user. Root is
// treated as owning everything, matching how the kernel resolves access.
func ownedByCurrentUser(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	uid := os.Getuid()
	return uid == 0 || uint32(uid) == st.Uid
}
