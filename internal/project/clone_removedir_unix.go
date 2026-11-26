//go:build linux || darwin

package project

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// removeDir removes name from the root by rmdir through the parent's
// descriptor: the kernel refuses a non-directory and a directory with
// contents, so a name swapped for a file or a link after the caller's
// identity check is never removed, and neither is anything that arrived
// in the directory since.
func removeDir(root *os.Root, name string) error {
	parent, err := root.OpenFile(filepath.Dir(name), os.O_RDONLY|leafOpenFlags, 0)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	for {
		err := unix.Unlinkat(int(parent.Fd()), filepath.Base(name), unix.AT_REMOVEDIR)
		runtime.KeepAlive(parent)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
