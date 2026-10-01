//go:build !unix

package main

import "os"

// lockFile is a no-op where flock doesn't exist; the bridge runs on Linux.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) error { return nil }
