//go:build unix

package main

import (
	"os"
	"syscall"
)

// configOwnedBy reports whether fi's owner uid is exactly callerUID. Unlike
// ownedByCurrentUser, root gets no exemption.
func configOwnedBy(fi os.FileInfo, callerUID int) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return uidMatches(st.Uid, callerUID)
}

func uidMatches(fileUID uint32, callerUID int) bool {
	return callerUID >= 0 && uint32(callerUID) == fileUID
}

// openConfigNoFollow opens path without following a final symlink, so the
// ownership check and the read see the same file.
func openConfigNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
