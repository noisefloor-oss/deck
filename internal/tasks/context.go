package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const taskContextSchemaVersion = 5

var ErrTaskStateBlocked = errors.New("taskmaster: task state is blocked")
var ErrTaskStateReadOnly = errors.New("taskmaster: task state is not writable")

// engineProjectContext is the engine's schema-5 project-context contract.
// The SQLite store at StorePath is the single task authority; it lives under
// the repository family's Git common directory, so it is the one context path
// allowed outside the project boundary.
type engineProjectContext struct {
	SchemaVersion       int    `json:"schemaVersion"`
	Start               string `json:"start"`
	ProjectBoundary     string `json:"projectBoundary"`
	StorePath           string `json:"storePath"`
	StoreOwnerBoundary  string `json:"storeOwnerBoundary"`
	StoreSchemaVersion  int    `json:"storeSchemaVersion"`
	EngineSchemaVersion int    `json:"engineSchemaVersion"`
	DocsPath            string `json:"docsPath"`
	PreferencesPath     string `json:"preferencesPath"`
	Initialized         bool   `json:"initialized"`
	Writable            bool   `json:"writable"`
	Blocked             bool   `json:"blocked"`
	BlockedReason       string `json:"blockedReason"`
}

type boundedBuffer struct {
	buf      bytes.Buffer
	overflow bool
	limit    int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	limit := b.limit
	if limit == 0 {
		limit = 64 << 10
	}
	n := len(p)
	remaining := limit - b.buf.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = b.buf.Write(p[:remaining])
	}
	if remaining < len(p) {
		b.overflow = true
	}
	return n, nil
}

// resolveEngineProjectContext resolves the engine context and confirms
// the schema-5 store family. Owner confirmation uses the raw resolver,
// bounded by this call's own deadline and never recursive.
func resolveEngineProjectContext(parent context.Context, executable, projectPath string) (*engineProjectContext, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	result, err := resolveEngineProjectContextRaw(ctx, executable, projectPath)
	if err != nil {
		return nil, err
	}
	// Schema 5 names the family that owns the resolved store. The
	// registered checkout may be the family's primary (owner equals the
	// registration) or one of its linked worktrees (the owner is the
	// primary); both legitimately share the family store. A claimed
	// owner that is not the registration is confirmed with exactly one
	// additional Raw resolution under this call's own deadline — never a
	// recursive chain — requiring that owner to resolve to itself and
	// to the same store: a cyclic cross-claim fails because neither
	// side owns itself, and a foreign or vanished owner fails closed.
	if result.StoreOwnerBoundary != "" {
		registered, err := filepath.EvalSymlinks(projectPath)
		if err != nil {
			return nil, errors.New("taskmaster: registered project boundary is unavailable")
		}
		owner, err := filepath.EvalSymlinks(result.StoreOwnerBoundary)
		if err != nil {
			return nil, fmt.Errorf("%w: task store owner boundary is unavailable", ErrUnsafeTree)
		}
		if owner != registered {
			ownerCtx, err := resolveEngineProjectContextRaw(ctx, executable, owner)
			if err != nil ||
				ownerCtx.StorePath != result.StorePath ||
				ownerCtx.ProjectBoundary != owner ||
				filepath.Clean(ownerCtx.StoreOwnerBoundary) != owner {
				return nil, fmt.Errorf("%w: task store is owned by another repository family", ErrUnsafeTree)
			}
		}
	}
	return result, nil
}

// resolveEngineProjectContextRaw resolves one context and validates
// every contract fact except the store's family ownership.
func resolveEngineProjectContextRaw(parent context.Context, executable, projectPath string) (*engineProjectContext, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "tasks", "context", "--json")
	cmd.Dir = projectPath
	cmd.Env = append(withoutEnvironment(os.Environ(), "NOISE_PROJECT_DIR"), "NOISE_PROJECT_DIR="+projectPath)
	var stdout, stderr boundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("taskmaster: task context timed out: %w", ctx.Err())
		}
		return nil, fmt.Errorf("taskmaster: task context command failed: %w", err)
	}
	if stdout.overflow || stderr.overflow {
		return nil, errors.New("taskmaster: task context output exceeded limit")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(stdout.buf.Bytes(), &raw); err != nil {
		return nil, errors.New("taskmaster: task context returned invalid JSON")
	}
	wantKeys := []string{"schemaVersion", "start", "projectBoundary", "storePath", "storeOwnerBoundary", "storeSchemaVersion", "engineSchemaVersion", "docsPath", "preferencesPath", "initialized", "writable", "blocked", "blockedReason"}
	if len(raw) != len(wantKeys) {
		return nil, errors.New("taskmaster: task context returned an unsupported shape")
	}
	for _, key := range wantKeys {
		if _, ok := raw[key]; !ok {
			return nil, errors.New("taskmaster: task context omitted a required field")
		}
	}
	var result engineProjectContext
	dec := json.NewDecoder(bytes.NewReader(stdout.buf.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		return nil, errors.New("taskmaster: task context returned an unsupported shape")
	}
	if result.SchemaVersion != taskContextSchemaVersion {
		return nil, fmt.Errorf("taskmaster: unsupported task context schema %d", result.SchemaVersion)
	}
	if err := validateEngineContextBlocked(&result); err != nil {
		return nil, err
	}
	boundary, err := filepath.EvalSymlinks(result.ProjectBoundary)
	if err != nil {
		return nil, errors.New("taskmaster: task context boundary is unavailable")
	}
	registered, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		return nil, errors.New("taskmaster: registered project boundary is unavailable")
	}
	if boundary != registered || result.ProjectBoundary != boundary {
		return nil, fmt.Errorf("%w: engine project boundary differs from the registered project", ErrUnsafeTree)
	}
	if result.Start != registered {
		return nil, fmt.Errorf("%w: engine context did not start at the registered project", ErrUnsafeTree)
	}
	// A store newer than the pinned engine is version skew the caller
	// cannot act on safely; the zero values never assert this.
	if result.EngineSchemaVersion != 0 && result.StoreSchemaVersion > result.EngineSchemaVersion {
		return nil, errors.New("taskmaster: task store schema is newer than the pinned engine; upgrade the Tasks pin")
	}
	if result.Blocked {
		return nil, fmt.Errorf("%w: %s", ErrTaskStateBlocked, result.BlockedReason)
	}
	if err := validateEngineContextPaths(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// validateEngineContextBlocked accepts only the pinned engine's migration and
// adoption recovery states. Either means publication did not complete, so
// callers must wait for engine recovery rather than choose task authority.
func validateEngineContextBlocked(c *engineProjectContext) error {
	if !c.Blocked {
		if c.BlockedReason != "" {
			return errors.New("taskmaster: unblocked task context carried a blocked reason")
		}
		return nil
	}
	switch c.BlockedReason {
	case "migration-recovery-required", "adoption-recovery-required":
		return nil
	default:
		return errors.New("taskmaster: task context returned an unsupported blocked reason")
	}
}

// validateEngineContextPaths pins the store to an absolute canonical path and
// the two worktree documents to clean relative paths. The store sits under
// the Git common directory by design, so it is never project-relative; a
// relative or unclean store path is an unsupported contract, not a state.
func validateEngineContextPaths(c *engineProjectContext) error {
	if !filepath.IsAbs(c.StorePath) || filepath.Clean(c.StorePath) != c.StorePath {
		return errors.New("taskmaster: task context returned an unsafe store path")
	}
	for _, name := range []string{c.DocsPath, c.PreferencesPath} {
		if filepath.IsAbs(name) || filepath.Clean(name) != name || strings.Contains(name, "\\") || name == "." || name == ".." || strings.HasPrefix(name, "../") {
			return errors.New("taskmaster: task context returned an unsafe relative path")
		}
	}
	return nil
}

func withoutEnvironment(env []string, key string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return result
}

// openEngineTaskRoot binds the registered project and stamps it with the
// engine-selected store, docs, and preferences paths. Every engine-selected
// project is live-authority shaped: the schema-4 store is the only task
// state, so reads, mutations, and events all go through the engine.
func openEngineTaskRoot(ctx context.Context, executable, projectPath string, afterAnchor func()) (*taskRoot, error) {
	selected, err := resolveEngineProjectContext(ctx, executable, projectPath)
	if err != nil {
		return nil, err
	}
	root, err := openTaskRoot(projectPath, afterAnchor)
	if err != nil {
		return nil, err
	}
	root.storePath = selected.StorePath
	root.preferencesPath = selected.PreferencesPath
	root.docsDir = filepath.Dir(selected.DocsPath)
	root.initialized = selected.Initialized
	root.writable = selected.Writable
	return root, nil
}
