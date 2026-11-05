//go:build linux

package tasks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func openTaskRoot(projectPath string, afterAnchor func()) (*taskRoot, error) {
	if !filepath.IsAbs(projectPath) || filepath.Clean(projectPath) != projectPath {
		return nil, fmt.Errorf("%w: project root is not canonical", ErrUnsafeTree)
	}

	fd, err := openTaskDirectory("/")
	if err != nil {
		return nil, fmt.Errorf("taskmaster: open project root anchor: %w", err)
	}
	for _, component := range strings.Split(strings.TrimPrefix(projectPath, "/"), "/") {
		if component == "" {
			continue
		}
		next, openErr := openTaskDirectoryAt(fd, component)
		closeErr := unix.Close(fd)
		if openErr != nil {
			if errors.Is(openErr, syscall.ELOOP) || errors.Is(openErr, syscall.ENOTDIR) || errors.Is(openErr, syscall.ENOENT) {
				return nil, fmt.Errorf("%w: project root changed during acquisition: %v", ErrUnsafeTree, openErr)
			}
			return nil, fmt.Errorf("taskmaster: open project root component: %w", openErr)
		}
		if closeErr != nil {
			unix.Close(next)
			return nil, fmt.Errorf("taskmaster: close project root component: %w", closeErr)
		}
		fd = next
	}

	anchor := os.NewFile(uintptr(fd), "task-project-root")
	if anchor == nil {
		unix.Close(fd)
		return nil, errors.New("taskmaster: invalid project root descriptor")
	}
	anchorInfo, err := anchor.Stat()
	if err != nil {
		anchor.Close()
		return nil, fmt.Errorf("taskmaster: stat project root anchor: %w", err)
	}
	if afterAnchor != nil {
		afterAnchor()
	}

	root, err := os.OpenRoot(procDescriptorPath(os.Getpid(), fd))
	if err != nil {
		anchor.Close()
		return nil, fmt.Errorf("taskmaster: bind project root anchor: %w", err)
	}
	probe, err := root.Open(".")
	if err != nil {
		root.Close()
		anchor.Close()
		return nil, fmt.Errorf("taskmaster: validate project root anchor: %w", err)
	}
	probeInfo, statErr := probe.Stat()
	closeErr := probe.Close()
	if statErr != nil {
		root.Close()
		anchor.Close()
		return nil, fmt.Errorf("taskmaster: validate project root anchor: %w", statErr)
	}
	if closeErr != nil {
		root.Close()
		anchor.Close()
		return nil, fmt.Errorf("taskmaster: close project root probe: %w", closeErr)
	}
	if !os.SameFile(anchorInfo, probeInfo) {
		root.Close()
		anchor.Close()
		return nil, errors.New("taskmaster: project root descriptor identity changed")
	}
	return &taskRoot{
		path: projectPath, anchor: anchor, root: root,
		docsDir: ".taskmaster/docs", initialized: true, writable: true,
	}, nil
}

func (r *taskRoot) Close() error {
	return errors.Join(r.root.Close(), r.anchor.Close())
}

func (r *taskRoot) descriptorPath() string {
	return procDescriptorPath(os.Getpid(), int(r.anchor.Fd()))
}

func descriptorPath(f *os.File) string {
	return procDescriptorPath(os.Getpid(), int(f.Fd()))
}

func procDescriptorPath(pid, fd int) string {
	return "/proc/" + strconv.Itoa(pid) + "/fd/" + strconv.Itoa(fd)
}

func openTaskDirectory(name string) (int, error) {
	for {
		fd, err := unix.Open(name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != syscall.EINTR {
			return fd, err
		}
	}
}

func openTaskDirectoryAt(parent int, name string) (int, error) {
	for {
		fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != syscall.EINTR {
			return fd, err
		}
	}
}

func openTaskRootDirectory(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
}
