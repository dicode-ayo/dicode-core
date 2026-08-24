//go:build !unix

package main

import "os"

// ownedByCurrentUser reports whether fi's owner is the calling user. Windows
// ACLs do not map onto a uid comparison, and the shared-host threat this gate
// addresses is a POSIX multi-user one, so the check passes here.
func ownedByCurrentUser(os.FileInfo) bool { return true }
