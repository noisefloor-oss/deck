//go:build darwin

package tasks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Darwin cannot reopen directory descriptors through /dev/fd with O_DIRECTORY.
// Acquire matching no-follow and os.Root handles at each directory component.
func openTaskRoot(projectPath string, afterAnchor func()) (*taskRoot, error) {
	if !filepath.IsAbs(projectPath) || filepath.Clean(projectPath) != projectPath {
		return nil, fmt.Errorf("%w: project root is not canonical", ErrUnsafeTree)
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	anchor := os.NewFile(uintptr(fd), "task-project-root")
	root, err := os.OpenRoot("/")
	if err != nil {
		anchor.Close()
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = root.Close()
			_ = anchor.Close()
		}
	}()
	for _, component := range strings.Split(strings.TrimPrefix(projectPath, "/"), "/") {
		if component == "" {
			continue
		}
		nextFD, err := openTaskDirectoryAt(int(anchor.Fd()), component)
		if err != nil {
			return nil, taskRootAcquisitionError(err)
		}
		nextAnchor := os.NewFile(uintptr(nextFD), "task-project-root")
		nextRoot, err := root.OpenRoot(component)
		if err != nil {
			nextAnchor.Close()
			return nil, taskRootAcquisitionError(err)
		}
		anchorInfo, anchorErr := nextAnchor.Stat()
		rootInfo, rootErr := nextRoot.Stat(".")
		if anchorErr != nil || rootErr != nil || !os.SameFile(anchorInfo, rootInfo) {
			nextRoot.Close()
			nextAnchor.Close()
			return nil, fmt.Errorf("%w: project root descriptor identity changed", ErrUnsafeTree)
		}
		oldRoot, oldAnchor := root, anchor
		root, anchor = nextRoot, nextAnchor
		if err := errors.Join(oldRoot.Close(), oldAnchor.Close()); err != nil {
			return nil, err
		}
	}
	if afterAnchor != nil {
		afterAnchor()
	}
	keep = true
	return &taskRoot{
		path: projectPath, anchor: anchor, root: root,
		docsDir: ".taskmaster/docs", initialized: true, writable: true,
	}, nil
}

func taskRootAcquisitionError(err error) error {
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("%w: project root changed during acquisition: %w", ErrUnsafeTree, err)
	}
	return fmt.Errorf("tasks: open project root component: %w", err)
}

func openTaskDirectoryAt(parent int, name string) (int, error) {
	for {
		fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != syscall.EINTR {
			return fd, err
		}
	}
}

func (r *taskRoot) Close() error { return errors.Join(r.root.Close(), r.anchor.Close()) }

// Darwin has no process-addressable descriptor paths.
func (r *taskRoot) descriptorPath() string { return "" }
func descriptorPath(*os.File) string       { return "" }

func openTaskRootDirectory(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
}
