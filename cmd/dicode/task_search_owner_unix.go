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
