//go:build !linux && !darwin

package project

import (
	"os"
	"syscall"
)

// removeDir is the best the unsupported-root tier can do: a directory
// check before the root's own remove. The confined platforms do this
// with unlinkat(AT_REMOVEDIR), which a swap cannot race; project roots
// are refused here before any of this runs.
func removeDir(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return syscall.ENOTDIR
	}
	return root.Remove(name)
}
