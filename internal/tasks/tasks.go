// Package taskmaster integrates task-backlog projects through the pinned
// noise tasks engine. The engine's schema-4 context names one authority per
// repository family: the SQLite store under the Git common directory. Task
// reads, mutations, and change events all go through the engine against that
// store; no file inside a Git worktree is task state.
//
// Generation jobs live in jobs.go; nothing in THIS file executes the engine.
//
// Document paths stay beneath the registered project's open directory.
// Relative links and absolute links into that directory are supported; a link
// that leaves the project is refused even if another link would lead back in.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// ErrNotTaskmasterProject reports a directory with no initialized task
// structure. It is a state, not a failure: the panel simply does not
// render for such projects.
var ErrNotTaskmasterProject = errors.New("taskmaster: not a taskmaster project")

// ErrUnsafeTree reports task state that resolves outside the
// registered project's canonical root. Unlike
// ErrNotTaskmasterProject this is a refusal, not a state.
var ErrUnsafeTree = errors.New("taskmaster: task state resolves outside the project root")

// ProjectInfo describes a detected Taskmaster project.
type ProjectInfo struct {
	Path       string `json:"path"`
	ConfigPath string `json:"config_path"`
	TasksPath  string `json:"tasks_path"`
	DocsPath   string `json:"docs_path"`
}

// Task is one task or subtask returned by the Tasks JSON contract. The shape
// is deliberately permissive: a field Deck does not read must not break the
// response.
type Task struct {
	ID           any        `json:"id"`
	ParentID     any        `json:"parentId,omitempty"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	Status       string     `json:"status"`
	Priority     string     `json:"priority,omitempty"`
	Dependencies []any      `json:"dependencies,omitempty"`
	Details      string     `json:"details,omitempty"`
	Subtasks     []Task     `json:"subtasks,omitempty"`
	UpdatedAt    string     `json:"updatedAt,omitempty"`
	Revision     int        `json:"revision,omitempty"`
	Claim        *TaskClaim `json:"claim,omitempty"`
}

// TaskClaim is the content-free claim view published by the Tasks engine.
// The fencing token is intentionally never returned by list.
type TaskClaim struct {
	Owner      string `json:"owner"`
	AcquiredAt string `json:"acquiredAt"`
	ExpiresAt  string `json:"expiresAt"`
}

// TagFile is one tag in the Tasks response served to the browser.
type TagFile struct {
	Tasks []Task `json:"tasks"`
}

// taskRoot keeps both the project directory and its os.Root traversal guard
// alive. The separate anchor descriptor binds the engine's working directory:
// a renamed project remains the same directory.
// storePath and preferencesPath are set only on engine-selected roots.
type taskRoot struct {
	path            string
	anchor          *os.File
	root            *os.Root
	docsDir         string
	storePath       string
	preferencesPath string
	initialized     bool
	writable        bool
}

// taskAccessHooks are deterministic adversarial-test boundaries. Production
// callers leave every hook nil.
type taskAccessHooks struct {
	rootAnchored func()
	targetOpened func()
	handoffReady func()
}

func mapTaskAccessError(err error) error {
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EXDEV) {
		return ErrUnsafeTree
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		var errno syscall.Errno
		if !errors.As(pathErr.Err, &errno) {
			return ErrUnsafeTree
		}
	}
	return err
}

// fallbackName resolves absolute in-project links through the open root.
// Host pathnames never select the directory used for metadata lookups.
func (r *taskRoot) fallbackName(name string) (string, error) {
	if !filepath.IsLocal(name) {
		return "", ErrUnsafeTree
	}
	pending := strings.Split(name, string(filepath.Separator))
	resolved := "."
	links := 0
	for len(pending) > 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if resolved == "." {
				return "", ErrUnsafeTree
			}
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, component)
		info, err := r.root.Lstat(next)
		if errors.Is(err, os.ErrNotExist) {
			return filepath.Join(append([]string{next}, pending...)...), nil
		}
		if err != nil {
			return "", mapTaskAccessError(err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			if len(pending) > 0 && !info.IsDir() {
				return "", syscall.ENOTDIR
			}
			resolved = next
			continue
		}
		links++
		if links > 255 {
			return "", ErrUnsafeTree
		}
		target, err := r.root.Readlink(next)
		if err != nil {
			return "", mapTaskAccessError(err)
		}
		if filepath.IsAbs(target) {
			prefix := strings.TrimRight(r.path, string(filepath.Separator)) + string(filepath.Separator)
			switch {
			case target == r.path:
				target = "."
			case strings.HasPrefix(target, prefix):
				target = strings.TrimPrefix(target, prefix)
			default:
				return "", ErrUnsafeTree
			}
			resolved = "."
		}
		pending = append(strings.Split(target, string(filepath.Separator)), pending...)
	}
	return resolved, nil
}

func (r *taskRoot) openFile(name string, flags int, perm os.FileMode) (*os.File, error) {
	f, err := r.root.OpenFile(name, flags, perm)
	if err == nil {
		return f, nil
	}
	fallback, resolveErr := r.fallbackName(name)
	if resolveErr != nil {
		return nil, resolveErr
	}
	if fallback == name {
		return nil, mapTaskAccessError(err)
	}
	f, err = r.root.OpenFile(fallback, flags, perm)
	if err != nil {
		return nil, mapTaskAccessError(err)
	}
	return f, nil
}

func (r *taskRoot) openDirectory(name string) (*os.File, error) {
	dir, err := openTaskRootDirectory(r.root, name)
	if err == nil {
		return dir, nil
	}
	fallback, resolveErr := r.fallbackName(name)
	if resolveErr != nil {
		return nil, resolveErr
	}
	if fallback == name {
		return nil, mapTaskAccessError(err)
	}
	dir, err = openTaskRootDirectory(r.root, fallback)
	if err != nil {
		return nil, mapTaskAccessError(err)
	}
	return dir, nil
}

func (r *taskRoot) stat(name string) (os.FileInfo, error) {
	info, err := r.root.Stat(name)
	if err == nil {
		return info, nil
	}
	fallback, resolveErr := r.fallbackName(name)
	if resolveErr != nil {
		return nil, resolveErr
	}
	if fallback == name {
		return nil, mapTaskAccessError(err)
	}
	info, err = r.root.Stat(fallback)
	if err != nil {
		return nil, mapTaskAccessError(err)
	}
	return info, nil
}

func (r *taskRoot) mkdirAll(name string, perm os.FileMode) error {
	if err := r.root.MkdirAll(name, perm); err == nil {
		return nil
	} else {
		fallback, resolveErr := r.fallbackName(name)
		if resolveErr != nil {
			return resolveErr
		}
		if fallback == name {
			return mapTaskAccessError(err)
		}
		return mapTaskAccessError(r.root.MkdirAll(fallback, perm))
	}
}

func (r *taskRoot) detectProject() (*ProjectInfo, error) {
	if r.storePath != "" {
		// Engine-selected root: the SQLite store is the only authority. An
		// uninitialized family has no task state at all — a state, not a
		// failure, so the panel hides itself.
		if !r.initialized {
			return nil, ErrNotTaskmasterProject
		}
		return &ProjectInfo{
			Path:       r.path,
			ConfigPath: filepath.Join(r.path, r.preferencesPath),
			TasksPath:  r.storePath,
			DocsPath:   filepath.Join(r.path, r.docsDir),
		}, nil
	}
	return nil, ErrNotTaskmasterProject
}

func DetectProjectWithEngine(ctx context.Context, executable, projectPath string) (*ProjectInfo, error) {
	root, err := openEngineTaskRoot(ctx, executable, projectPath, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.detectProject()
}

func ReadTasksWithEngine(ctx context.Context, executable, projectPath string) (map[string]TagFile, error) {
	root, err := openEngineTaskRoot(ctx, executable, projectPath, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if _, err := root.detectProject(); err != nil {
		return nil, err
	}
	return readLiveTasks(ctx, executable, root)
}

func ListPRDFilesWithEngine(ctx context.Context, executable, projectPath string) ([]string, error) {
	root, err := openEngineTaskRoot(ctx, executable, projectPath, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if _, err := root.detectProject(); err != nil {
		return nil, err
	}
	return root.listPRDFiles(taskAccessHooks{})
}

func (root *taskRoot) listPRDFiles(hooks taskAccessHooks) ([]string, error) {
	docsDir, err := root.openDirectory(root.docsDir)
	if err != nil {
		if errors.Is(err, ErrUnsafeTree) {
			return nil, err
		}
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("taskmaster: read docs dir: %w", err)
	}
	defer docsDir.Close()
	if hooks.targetOpened != nil {
		hooks.targetOpened()
	}
	entries, err := docsDir.ReadDir(-1)
	if err != nil {
		// A genuinely absent docs dir is an empty list, not an error.
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("taskmaster: read docs dir: %w", err)
	}
	var prds []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".md" {
			prds = append(prds, e.Name())
		}
	}
	sort.Strings(prds)
	if prds == nil {
		prds = []string{}
	}
	return prds, nil
}

func WritePRDFileWithEngine(ctx context.Context, executable, projectPath, name, content string, replace bool) error {
	root, err := openEngineTaskRoot(ctx, executable, projectPath, nil)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return writePRDFileAtRoot(root, name, content, replace, taskAccessHooks{})
}

func writePRDFileAtRoot(root *taskRoot, name, content string, replace bool, hooks taskAccessHooks) error {
	if name == "" || filepath.Base(name) != name || filepath.Ext(name) != ".md" ||
		name != strings.TrimSpace(name) || strings.HasPrefix(name, ".") {
		return fmt.Errorf("%w: a PRD name must be a plain .md filename", ErrInvalidPRD)
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("%w: empty PRD content", ErrInvalidPRD)
	}
	if len(content) > 1<<20 {
		return fmt.Errorf("%w: PRD content over 1 MiB", ErrInvalidPRD)
	}
	if !root.writable {
		return ErrTaskStateReadOnly
	}
	if err := root.mkdirAll(root.docsDir, 0o750); err != nil {
		return fmt.Errorf("taskmaster: create docs dir: %w", err)
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if replace {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := root.openFile(filepath.Join(root.docsDir, name), flags, 0o600)
	if err != nil {
		if !replace && os.IsExist(err) {
			return fmt.Errorf("%w: %s", ErrPRDExists, name)
		}
		return fmt.Errorf("taskmaster: write PRD: %w", err)
	}
	defer func() { _ = f.Close() }()
	if hooks.targetOpened != nil {
		hooks.targetOpened()
	}
	if _, err := f.WriteString(content); err != nil {
		return fmt.Errorf("taskmaster: write PRD: %w", err)
	}
	return nil
}
