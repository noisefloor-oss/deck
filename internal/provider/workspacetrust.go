package provider

// First-run workspace trust.
//
// Claude, Codex and Kimi each record, in their own state, whether a directory
// has been trusted. This reads that record for one project so a caller can
// see which harnesses are provisioned for it, and which file carries the
// answer.
//
// It reports. Nothing here gates a launch, and no result asserts anything
// about a session: a config file cannot establish whether a CLI will stop at
// a prompt. The identity-hook check beside it is a diagnostic for the same
// reason — these are CLIs Deck does not own, whose state schemas change
// upstream on their own schedule.
//
// Deck never writes these files. Reading them is the whole of what it
// does here, and no uncertainty is ever reported as a gate: every one resolves to
// WorkspaceTrustUnknown, so a schema that changes upstream costs a diagnostic
// rather than a working launch.
//
// The gates, and how each one decides, are recorded in the contracts
// (docs/provider-contracts/{claude,codex,kimi}.yaml) with the versions they
// were measured on. Three properties this code depends on:
//
//   - Claude and Codex INHERIT: a directory under a trusted one launches
//     without a modal, and Codex also carries the decision from a linked
//     worktree's main checkout, which is not an ancestor of it. So an
//     exact-key miss is not evidence of a modal: the candidates are the
//     path, every directory above it, and the main checkout of a worktree
//     it lives in.
//   - Kimi does NOT inherit, and its record is one file per exact directory.
//   - A recorded REFUSAL is not the same thing in both: Claude asks again
//     when its entry says the dialog was not accepted, while Codex takes an
//     untrusted project as decided and starts without a prompt.
//
// The asymmetry is deliberate. Reporting a decision the provider would not
// have consulted says nothing a user needs to act on; reporting a gate
// that is not there sends them after a file they do not need, and invites a
// caller to work around a problem it does not have. Every rule here is
// therefore the WEAKER one: prepared wherever the provider might have an
// answer, absent only when nothing in the record covers the path.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// WorkspaceTrustState is what the provider's own record says about one
// project path.
type WorkspaceTrustState int

const (
	// WorkspaceTrustUnknown covers every case with no answer to report: a
	// harness whose gate is unmeasured, a relocated or relative provider home
	// this has not been measured against, a project path that is not the
	// absolute directory a launch passes, and a record that cannot be read or
	// parsed. It always carries a reason.
	WorkspaceTrustUnknown WorkspaceTrustState = iota
	// WorkspaceTrustPrepared means the record covers this path: the provider
	// has an answer for it, or for a directory it takes the answer from. It
	// is the deliberately broad side of the classification.
	WorkspaceTrustPrepared
	// WorkspaceTrustAbsent means nothing in the record covers this path. For
	// Claude and Codex that is a record that was read; for Kimi it is also a
	// missing per-directory file, which is the form its answer takes.
	WorkspaceTrustAbsent
)

// WorkspaceTrustResult is one answer, with the file that carries it. The path
// is what the readout names — a user cannot act on "trust is missing".
type WorkspaceTrustResult struct {
	State WorkspaceTrustState
	// RecordPath is the provider-owned file the answer came from, or the one
	// a prepared record would live in. Empty when nothing was measured.
	RecordPath string
	// Reason names why the state is Unknown, for a diagnostic. Empty
	// otherwise.
	Reason string
}

// TrustHomes are the provider homes to read, resolved from the same
// environment a launch inherits (PermittedEnvironment carries HOME plus each
// provider's own override). Tests pass fixture roots.
type TrustHomes struct {
	Home string
	// ClaudeConfigDir is CLAUDE_CONFIG_DIR. Claude's trust record was
	// measured at $HOME/.claude.json only; where a relocated config dir puts
	// it is unmeasured, so ANY value here yields Unknown rather than a guess.
	ClaudeConfigDir string
	// CodexHome is CODEX_HOME; empty means $HOME/.codex.
	CodexHome string
	// KimiCodeHome is KIMI_CODE_HOME; empty means $HOME/.kimi-code.
	KimiCodeHome string
}

// TrustHomesFromEnvironment reads the homes a launch would use, byte for
// byte: launchArgv passes what the environment holds, and
// "/srv/codex " with a trailing space is a different directory from
// "/srv/codex" — trimming here would answer about a config the launch does
// not read.
func TrustHomesFromEnvironment() TrustHomes {
	return TrustHomes{
		Home:            os.Getenv("HOME"),
		ClaudeConfigDir: os.Getenv("CLAUDE_CONFIG_DIR"),
		CodexHome:       os.Getenv("CODEX_HOME"),
		KimiCodeHome:    os.Getenv("KIMI_CODE_HOME"),
	}
}

// providerHome resolves one provider's home from its override and $HOME. A
// RELATIVE override resolves against the process's working directory, and the
// launch's is the project — two different directories — so it is an
// uncertainty, not a path to guess at.
func (h TrustHomes) providerHome(override, homeSuffix string) (string, string) {
	if override != "" {
		if !filepath.IsAbs(override) {
			return "", "the provider home override is relative and resolves against a working directory this cannot know"
		}
		return override, ""
	}
	if h.Home == "" {
		return "", "no HOME to resolve the provider config"
	}
	if !filepath.IsAbs(h.Home) {
		return "", "HOME is relative and resolves against a working directory this cannot know"
	}
	return filepath.Join(h.Home, homeSuffix), ""
}

// ancestors lists the path and every directory above it, nearest first.
func ancestors(path string) []string {
	clean := filepath.Clean(path)
	out := []string{clean}
	for {
		parent := filepath.Dir(clean)
		if parent == clean {
			return out
		}
		out = append(out, parent)
		clean = parent
	}
}

// trustCandidates is every directory a record could carry this path's answer
// in: the path, everything above it, and — for a linked git worktree — the
// main checkout and everything above THAT. A worktree lives wherever it was
// created, so the checkout that holds the decision need not be an ancestor.
//
// Adding candidates can only move an answer towards prepared, which is the
// safe direction for a readout: over-reporting a decision says nothing new,
// while reporting a gate that is not there sends a caller after a file it
// does not need.
func trustCandidates(path string) []string {
	out := ancestors(path)
	if checkout, ok := worktreeMainCheckout(path); ok {
		out = append(out, ancestors(checkout)...)
	}
	return out
}

// worktreeMainCheckout resolves the main checkout of the linked worktree path
// lives in, if any. A linked worktree's .git is a FILE naming its private
// git directory, and commondir beside it points at the shared one; the
// checkout is that directory's parent. Anything unexpected answers "no
// worktree" rather than guessing at a directory.
func worktreeMainCheckout(path string) (string, bool) {
	for _, dir := range ancestors(path) {
		info, err := os.Stat(filepath.Join(dir, ".git"))
		if err != nil || info.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Clean(filepath.Join(dir, ".git")))
		if err != nil {
			return "", false
		}
		gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
		if !ok {
			return "", false
		}
		gitDir = strings.TrimSpace(gitDir)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(dir, gitDir)
		}
		common, err := os.ReadFile(filepath.Clean(filepath.Join(gitDir, "commondir")))
		if err != nil {
			return "", false
		}
		shared := strings.TrimSpace(string(common))
		if !filepath.IsAbs(shared) {
			shared = filepath.Join(gitDir, shared)
		}
		return filepath.Dir(filepath.Clean(shared)), true
	}
	return "", false
}

// WorkspaceTrust reports whether the provider's record covers a launch in
// projectPath. The path is compared as the launch will pass it — these CLIs
// key their record on the working directory they were given, so a caller that
// resolved symlinks here would ask about a directory the launch never names.
func (h TrustHomes) WorkspaceTrust(name Name, projectPath string) WorkspaceTrustResult {
	if projectPath == "" || !filepath.IsAbs(projectPath) {
		// The record is keyed on the absolute working directory the launch
		// passes; anything else cannot be looked up in it.
		return WorkspaceTrustResult{Reason: "project path is not absolute"}
	}
	switch name {
	case Claude:
		return h.claudeTrust(projectPath)
	case Codex:
		return h.codexTrust(projectPath)
	case Kimi:
		return h.kimiTrust(projectPath)
	case Devin:
		return h.devinTrust(projectPath)
	default:
		return WorkspaceTrustResult{Reason: fmt.Sprintf("%s has no measured workspace-trust gate", name)}
	}
}

// claudeTrust reads ~/.claude.json, whose projects map carries one entry per
// directory Claude has been started in; hasTrustDialogAccepted is the answer
// to the modal.
func (h TrustHomes) claudeTrust(projectPath string) WorkspaceTrustResult {
	if h.ClaudeConfigDir != "" {
		return WorkspaceTrustResult{Reason: "CLAUDE_CONFIG_DIR is set and the relocated trust record is unmeasured"}
	}
	home, reason := h.providerHome("", "")
	if reason != "" {
		return WorkspaceTrustResult{Reason: reason}
	}
	path := filepath.Join(home, ".claude.json")
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return unreadable(path, err)
	}
	var config struct {
		Projects map[string]struct {
			HasTrustDialogAccepted *bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return WorkspaceTrustResult{RecordPath: path, Reason: "claude config is not the measured shape: " + err.Error()}
	}
	// Only an accepted dialog counts: an entry recording that it was NOT
	// accepted asks again (measured), unlike Codex below.
	trusted := map[string]bool{}
	for key, entry := range config.Projects {
		if entry.HasTrustDialogAccepted != nil && *entry.HasTrustDialogAccepted {
			trusted[filepath.Clean(key)] = true
		}
	}
	for _, candidate := range trustCandidates(projectPath) {
		if trusted[candidate] {
			return WorkspaceTrustResult{State: WorkspaceTrustPrepared, RecordPath: path}
		}
	}
	return WorkspaceTrustResult{State: WorkspaceTrustAbsent, RecordPath: path}
}

// codexTrust reads CODEX_HOME/config.toml, whose [projects.'<path>'] table
// carries trust_level.
func (h TrustHomes) codexTrust(projectPath string) WorkspaceTrustResult {
	home, reason := h.providerHome(h.CodexHome, ".codex")
	if reason != "" {
		return WorkspaceTrustResult{Reason: reason}
	}
	path := filepath.Join(home, "config.toml")
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return unreadable(path, err)
	}
	var config struct {
		Projects map[string]struct {
			TrustLevel string `toml:"trust_level"`
		} `toml:"projects"`
	}
	if err := toml.Unmarshal(raw, &config); err != nil {
		return WorkspaceTrustResult{RecordPath: path, Reason: "codex config is not the measured shape: " + err.Error()}
	}
	// Any recorded decision ends the prompt, not just a trusting one: a
	// project recorded as untrusted starts without asking (measured), and
	// rewriting that user decision into a missing answer would report a
	// gate Codex does not have.
	decided := map[string]bool{}
	for key, entry := range config.Projects {
		if entry.TrustLevel != "" {
			decided[filepath.Clean(key)] = true
		}
	}
	for _, candidate := range trustCandidates(projectPath) {
		if decided[candidate] {
			return WorkspaceTrustResult{State: WorkspaceTrustPrepared, RecordPath: path}
		}
	}
	return WorkspaceTrustResult{State: WorkspaceTrustAbsent, RecordPath: path}
}

// kimiTrust reads one file per trusted directory under
// <kimi home>/workspace-trust. The NAME is load-bearing: Kimi looks up
// wd_<base name>_<first 12 hex of sha256(path)> and ignores a record filed
// under any other name (measured — renaming a working record brought the
// modal back), so the file this names is the only one that can answer.
func (h TrustHomes) kimiTrust(projectPath string) WorkspaceTrustResult {
	home, reason := h.providerHome(h.KimiCodeHome, ".kimi-code")
	if reason != "" {
		return WorkspaceTrustResult{Reason: reason}
	}
	path := filepath.Join(home, "workspace-trust", KimiWorkspaceTrustFileName(projectPath))
	raw, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		// THE ONE PLACE a missing file is an answer rather than an
		// uncertainty. Kimi files one record per exact directory and does not
		// inherit (measured: a subdirectory of a trusted project met the
		// modal), and a record under any other name left the modal in place,
		// so with a home present there is nowhere else the answer could be. A
		// home that does not exist is an installation nothing was measured on.
		if _, statErr := os.Stat(home); statErr != nil {
			return WorkspaceTrustResult{RecordPath: path, Reason: "kimi has no home yet"}
		}
		return WorkspaceTrustResult{State: WorkspaceTrustAbsent, RecordPath: path}
	}
	if err != nil {
		return unreadable(path, err)
	}
	var record struct {
		Root string `json:"root"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return WorkspaceTrustResult{RecordPath: path, Reason: "kimi trust record is not the measured shape: " + err.Error()}
	}
	// The name is a digest of the path, so a record naming a different root
	// is a collision or a stale file, not this project's answer.
	if record.Root != projectPath {
		return WorkspaceTrustResult{State: WorkspaceTrustAbsent, RecordPath: path}
	}
	return WorkspaceTrustResult{State: WorkspaceTrustPrepared, RecordPath: path}
}

// devinTrust reads the one central list Devin keeps: trusted_paths in
// <home>/.local/share/devin/cli/trusted_workspaces.json. Unlike Kimi there
// is no per-directory file and no load-bearing name, and unlike both Kimi
// and a bare exact-match reading, Devin INHERITS: a launch in a
// subdirectory of a listed path starts with no modal (measured on
// 3000.11.205 — a child of a trusted directory opened straight into the
// REPL), so any ancestor in the list covers the project.
func (h TrustHomes) devinTrust(projectPath string) WorkspaceTrustResult {
	home, reason := h.providerHome("", "")
	if reason != "" {
		return WorkspaceTrustResult{Reason: reason}
	}
	dir := filepath.Join(home, ".local", "share", "devin", "cli")
	path := filepath.Join(dir, "trusted_workspaces.json")
	raw, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		// A missing devin home is an installation nothing was measured on;
		// a present home without the file means Devin has never trusted
		// anything, which is an answer, not an uncertainty.
		if _, statErr := os.Stat(dir); statErr != nil {
			return WorkspaceTrustResult{RecordPath: path, Reason: "devin has no home yet"}
		}
		return WorkspaceTrustResult{State: WorkspaceTrustAbsent, RecordPath: path}
	}
	if err != nil {
		return unreadable(path, err)
	}
	var record struct {
		TrustedPaths []string `json:"trusted_paths"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return WorkspaceTrustResult{RecordPath: path, Reason: "devin trust record is not the measured shape: " + err.Error()}
	}
	for _, trusted := range record.TrustedPaths {
		if trusted == projectPath || isAncestorDirectory(trusted, projectPath) {
			return WorkspaceTrustResult{State: WorkspaceTrustPrepared, RecordPath: path}
		}
	}
	return WorkspaceTrustResult{State: WorkspaceTrustAbsent, RecordPath: path}
}

// isAncestorDirectory reports whether parent is a directory strictly above
// child on the cleaned path — the inheritance Devin and the CLIs that carry
// a decision downward consult. The root is an ancestor like any other: a
// record that lists "/" covers every path, which is the same total trust
// answering the modal in "/" itself would record.
func isAncestorDirectory(parent, child string) bool {
	if parent == "" {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// KimiWorkspaceTrustFileName is the exact name Kimi files a directory's trust
// record under. Exported because provisioning writes these records and must
// use the same name; a record under any other name is invisible to Kimi.
func KimiWorkspaceTrustFileName(projectPath string) string {
	sum := sha256.Sum256([]byte(projectPath))
	return "wd_" + filepath.Base(projectPath) + "_" + hex.EncodeToString(sum[:])[:12]
}

// unreadable separates "the record says this path is not trusted" from "there
// is no record to read". A provider whose config has never been written has
// nothing to say about this project, and a gate reported from that would be
// a gate Deck invented.
func unreadable(path string, err error) WorkspaceTrustResult {
	if errors.Is(err, fs.ErrNotExist) {
		return WorkspaceTrustResult{RecordPath: path, Reason: "no trust record file yet"}
	}
	return WorkspaceTrustResult{RecordPath: path, Reason: "trust record unreadable: " + err.Error()}
}
