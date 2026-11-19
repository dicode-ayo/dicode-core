//go:build !unix

package main

import "os"

// configOwnedBy passes on platforms without POSIX uids, as ownedByCurrentUser
// does.
func configOwnedBy(os.FileInfo, int) bool { return true }

func openConfigNoFollow(path string) (*os.File, error) { return os.Open(path) }
