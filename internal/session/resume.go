package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/noisefloor-oss/deck/internal/hookcapture"
	"github.com/noisefloor-oss/deck/internal/inputgate"
	"github.com/noisefloor-oss/deck/internal/provider"
	"github.com/noisefloor-oss/deck/internal/tmux"
)

type LeaseInvalidator interface {
	Invalidate(ctx context.Context, sessionID, reason string) error
}

type WorkDirResolver func(ctx context.Context, projectID string) (string, error)

func (m *Manager) SetResumeDeps(li LeaseInvalidator, gate *inputgate.Gate,
	workDir WorkDirResolver) {
	m.leases, m.gate, m.workDir = li, gate, workDir
}

const (
	recoveryGateBound    = 30 * time.Second
	recoveryCleanupBound = 8 * time.Second
)

// errAmbiguousCandidate marks an observation that cannot name one candidate
// pane. It is distinct from absence: absence proves there is nothing to
// converge and authorizes cleanup, while ambiguity authorizes nothing.
var errAmbiguousCandidate = errors.New("session: candidate pane is ambiguous")

// abortedByContext reports whether a recovery error is the CALLER's
// schedule ending the flight — the sweep budget, a shutdown — rather than
// an answer from the provider or the environment. Only the schedule's own
// aborts give the admission back: everything else is a real answer the
// user owns.
func abortedByContext(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func launchShape(s *Session, desc provider.Descriptor) (noIdentity, freshLaunch bool) {
	noIdentity = s.NativeSessionID == nil || *s.NativeSessionID == ""
	if desc.IdentityContract() == provider.IdentityNone ||
		desc.IdentityContract() == provider.IdentityFreshRelaunch {
		return noIdentity, true
	}
	// Carrying no usable identity is a reason to start a new conversation, not
	// a reason to abandon the session. Recovery only ever runs on a row whose
	// process is already gone, so an identity that has not arrived by now
	// never will: whatever would have delivered it died. Nothing is left to
	// resume, but every provider still opens a new conversation when it is
	// invoked with no resume argument, and launching that beats the only other
	// option — a card the user cannot act on at all, attached to a harness
	// that works perfectly from a shell.
	//
	// The caller reports this as RecoveryCapabilityRelaunch, never as a
	// resume, so nobody is told their old thread came back.
	return noIdentity, noIdentity
}

// RefusedResumeWindow is how long a resumed generation must survive before an
// unattended sweep may resume it again after it dies.
//
// A provider that does not recognise an id fails on the way up: it prints its
// complaint and exits. It does not run for a while and then discover the
// problem. Measured in production, omp answered `Error: Session "..." not
// found.` and was gone in about four seconds; claude 2.1.261 answers `No
// conversation found with session ID: ...` and exits 1 in about one second.
// A resume the provider accepts lives for as long as the user keeps
// working.
//
// The span measured is launch to OBSERVATION, not launch to death, because
// tmux reports that a pane is dead without saying when it died. Observation
// therefore inflates every measurement by up to one reconciler cadence, which
// can only hide a refusal, never invent one. The window is sized to swallow
// that inflation with room to spare.
//
// The judgement is deliberately weak, and its only consequence is to stop the
// unattended sweep. Claude shows its workspace-trust, onboarding and login
// dialogs BEFORE it looks the id up, so a refusal can arrive minutes after
// launch and read as an ordinary crash here; and a crash a few seconds into a
// real resumed conversation reads as a refusal. Neither misreading may cost
// the conversation, so the window never forgets an identity and never
// replaces the conversation with a fresh one — it only decides whether the
// sweep asks the provider again on its own or leaves the answer to the
// user's card.
const RefusedResumeWindow = 60 * time.Second

// refusedResume reports whether a generation that was launched by resuming a
// recorded identity died fast enough to read as the provider rejecting it.
//
// The evidence is the process, never the provider's output: output is
// presentation, it is reworded for cosmetic reasons, and it goes silent
// exactly when a CLI decides its errors belong on a different stream. How long
// the process lived is a fact about the process.
//
// A generation with no recorded start time is not judged. Rows written before
// that column existed have no lifetime to measure, and an unknown lifetime
// must never be read as a short one.
// A clean exit is never a refusal. A user who resumes a session, reads
// one answer and types /exit inside the window has been served exactly what
// they asked for, and their conversation must still be there next time. Only a
// nonzero status or a signal is the provider failing on the way up.
//
// A stop the user requested is not a refusal either: the process died
// because they said so, and how quickly it obeyed says nothing about the id.
func refusedResume(s *Session, code *int, signal *string, now time.Time) bool {
	if s == nil || !s.GenerationResumed || s.GenerationStartedAt == nil {
		return false
	}
	if s.NativeSessionID == nil || *s.NativeSessionID == "" {
		return false
	}
	if s.StopRequestedAt != nil || workspaceStopped(s) {
		return false
	}
	failed := signal != nil || (code != nil && *code != 0)
	if !failed {
		return false
	}
	return now.Sub(*s.GenerationStartedAt) < RefusedResumeWindow
}

func (m *Manager) RecoveryCapabilityFor(s *Session) RecoveryCapability {
	desc, ok := m.providers[provider.Name(s.Provider)]
	if !ok {
		return RecoveryCapabilityNone
	}
	return recoveryCapability(s, desc)
}

func recoveryCapability(s *Session, desc provider.Descriptor) RecoveryCapability {
	// Recovery replaces a corpse. A session that is still running has nothing
	// to recover, and offering to relaunch one invites a user to destroy
	// live work — including a session whose identity is merely still in
	// flight, which a late hook can still publish.
	if !terminalRecoverySource(s) {
		return RecoveryCapabilityNone
	}
	noIdentity, freshLaunch := launchShape(s, desc)
	if freshLaunch {
		// The probe asks only whether the descriptor can build fresh argv.
		// An assigned-identity provider requires an id in that argv, so the
		// probe supplies a syntactically valid placeholder — recovery itself
		// mints a real fresh uuid per candidate (assigned ids are single-use)
		// and this one is never launched.
		spec := provider.LaunchSpec{}
		if desc.IdentityContract() == provider.IdentityAssigned {
			spec.SessionID = recoveryProbeSessionID
		}
		argv, err := desc.NewSessionArgs(spec)
		if err != nil || len(argv) == 0 {
			return RecoveryCapabilityNone
		}
		return RecoveryCapabilityRelaunch
	}
	if noIdentity {
		return RecoveryCapabilityNone
	}
	argv, err := desc.ResumeSessionArgs(provider.ResumeSpec{NativeSessionID: *s.NativeSessionID})
	if err != nil || len(argv) == 0 {
		return RecoveryCapabilityNone
	}
	return RecoveryCapabilityResume
}

func terminalRecoverySource(s *Session) bool {
	return s != nil && s.RemovedAt == nil &&
		(s.Status == StatusExited || s.Status == StatusInterrupted)
}

func intentionalRecoveryExit(s *Session, desc provider.Descriptor, configured bool) bool {
	if s == nil || s.Status != StatusExited || s.ExitCode == nil {
		return false
	}
	return *s.ExitCode == 0 ||
		(configured && desc.IdentityContract() == provider.IdentityNone)
}

// maxAutomaticRecoveries bounds how many times the unattended sweep may
// relaunch one session inside automaticRecoveryWindow.
//
// ResumeAttempts alone bounds nothing across generations: a committed recovery
// clears it, because a generation that reached verified-live and was then lost
// has earned another attempt. That reasoning holds for one lost generation and
// fails for a provider that starts, commits, and exits again — it is relaunched
// once per reconciliation interval, forever, while nobody is watching. A
// production pi session reached generation 2881 and a cline session 3594 this
// way, each burning a provider launch every fifteen seconds.
//
// The window is what keeps the bound from punishing a healthy long-lived
// session: two relaunches in ten minutes is a loop, two over a week is not.
const (
	maxAutomaticRecoveries  = 2
	automaticRecoveryWindow = 10 * time.Minute
)

// automaticRecoveryExhausted reports whether this row has already spent its
// automatic recoveries inside the current window. A series older than the
// window is not a series any more.
func automaticRecoveryExhausted(s *Session, now time.Time) bool {
	if s == nil || s.AutomaticRecoverySeriesAt == nil {
		return false
	}
	if now.Sub(*s.AutomaticRecoverySeriesAt) >= automaticRecoveryWindow {
		return false
	}
	return s.AutomaticRecoveries >= maxAutomaticRecoveries
}

func automaticRecoveryOpportunity(s *Session, desc provider.Descriptor, configured bool,
	now time.Time) bool {
	return terminalRecoverySource(s) && s.StopRequestedAt == nil && !workspaceStopped(s) &&
		s.ResumeAttempts == 0 && completeVerifiedGeneration(s) &&
		!automaticRecoveryExhausted(s, now) &&
		!intentionalRecoveryExit(s, desc, configured)
}

// operatorRecoveryOpportunity gates the user's explicit resume. The
// automatic gate exists to bound an unattended loop and to honor an exit
// the user chose - protections against nobody watching. A person
// clicking Resume IS the watcher: a clean exit, a stopped session, or a
// spent automatic budget is still theirs to revive. Only a row that is
// not terminal (live, or removed) or that never completed a verified
// generation - nothing recorded to resume - refuses.
func operatorRecoveryOpportunity(s *Session) bool {
	return terminalRecoverySource(s) && completeVerifiedGeneration(s)
}

func recoverySessionName(prefix, sessionID, marker string) string {
	compactID := strings.ReplaceAll(sessionID, "-", "")
	if len(compactID) > 8 {
		compactID = compactID[:8]
	}
	return fmt.Sprintf("%s%s-r-%s", prefix, compactID,
		strings.ReplaceAll(marker, "-", ""))
}

// reusableRecoveryPane reports the session's own pane when it still holds this
// row's recorded corpse, and nil otherwise. Respawning destroys whatever
// occupies a pane, so this decides a destructive act and answers to the same
// standard as a kill: the full recorded identity, or nothing.
//
// Two conditions carry that weight. The pane must be DEAD, because a live pane
// is somebody's running work — ours or a user's — and recovery exists to
// replace a corpse, not to evict a process. And identity is checked through
// VerifyIdentity rather than by hand, because marker and generation are pane
// OPTIONS that survive an out-of-band respawn: a user who reclaimed this
// pane for their own command still answers to the recorded marker, and only
// the pane pid tmux retains from the exit distinguishes them. A pane failing
// either condition is not refused recovery — the caller gives the row a new
// home instead, which costs a tmux session and destroys nothing.
func (m *Manager) reusableRecoveryPane(ctx context.Context, s *Session) *tmux.PaneInfo {
	if s == nil || s.TmuxPaneID == nil || *s.TmuxPaneID == "" {
		return nil
	}
	info, err := m.tmux.PaneInfoByID(ctx, *s.TmuxPaneID)
	if err != nil || info == nil || info.SessionName == "" {
		return nil
	}
	if !info.Dead || VerifyIdentity(s, info) != nil {
		return nil
	}
	return info
}

// DeadPaneScreen returns the visible screen of the row's own pane when that
// pane still holds this row's recorded corpse — the same exact-identity test
// recovery applies before it respawns a pane. ErrNotFound means there is no
// retained screen to show: the row is live, the pane is gone, or it holds
// someone else's process.
func (m *Manager) DeadPaneScreen(ctx context.Context, s *Session) (string, error) {
	if !terminalRecoverySource(s) {
		return "", ErrNotFound
	}
	pane := m.reusableRecoveryPane(ctx, s)
	if pane == nil {
		return "", ErrNotFound
	}
	return m.tmux.CapturePaneTail(ctx, pane.PaneID, deadPaneScreenLines)
}

// deadPaneScreenLines bounds the retained output served to the card: enough
// to hold a provider's final complaint and the screen it printed on, small
// enough that a long-lived pane's history never becomes a response body.
const deadPaneScreenLines = 200

func recoveryExpectedExecutable(desc provider.Descriptor) (string, bool) {
	expected, err := expectedExecutable(desc, desc.Executable())
	return expected, err == nil
}

// resume is retained only as a same-package test helper. Product recovery is
// automatic; there is no manual nonce, replay endpoint, or second candidate.
func (m *Manager) resume(ctx context.Context, id, workDir string) (*Session, error) {
	return m.recover(ctx, id, workDir, false)
}

func (m *Manager) recover(ctx context.Context, id, suppliedWorkDir string,
	operator bool) (*Session, error) {
	if m.gate == nil || m.leases == nil {
		return nil, errors.New("session: recovery fencing is not wired")
	}
	done, err := m.gate.Admit()
	if err != nil {
		return nil, err
	}
	defer done()
	return m.recoverAdmitted(ctx, id, suppliedWorkDir, operator)
}

func (m *Manager) recoverAdmitted(ctx context.Context, id, suppliedWorkDir string,
	operator bool) (rs *Session, rerr error) {
	if m.leases == nil {
		return nil, errors.New("session: recovery fencing is not wired")
	}
	gate := m.gate.Session(id)
	gate.Lock()
	defer gate.Unlock()
	if m.lifecycleHooks != nil && m.lifecycleHooks.resumeGated != nil {
		m.lifecycleHooks.resumeGated()
	}
	gctx, cancel := context.WithTimeout(ctx, recoveryGateBound)
	defer cancel()

	source, err := m.repo.Get(gctx, id)
	if err != nil {
		return nil, err
	}
	// A recovery starts a process in the project like a create does, so a
	// project held against new sessions refuses it too, and a hold waits for
	// a recovery already admitted.
	endFence, err := m.fence.admit(source.ProjectID)
	if err != nil {
		return nil, err
	}
	defer endFence()
	desc, configured := m.providers[provider.Name(source.Provider)]
	if operator {
		if !operatorRecoveryOpportunity(source) {
			return nil, ErrNotResumable
		}
	} else if !automaticRecoveryOpportunity(source, desc, configured, m.now()) {
		return nil, ErrNotResumable
	}
	// A still-pending capture grant on the LOST generation does not withhold
	// recovery. Grants outlive the process they were minted for by design, so
	// a launch whose identity hook never fired leaves one pending until it
	// ages out; refusing recovery on that basis would make a silent hook cost
	// the session twice.

	workDir := suppliedWorkDir
	if workDir == "" {
		if m.workDir == nil {
			return nil, errors.New("session: recovery project resolver is unavailable")
		}
		workDir, err = m.workDir(gctx, source.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("session %s: resolve recovery project: %w", id, err)
		}
	}
	marker, err := m.newID()
	if err != nil {
		return nil, fmt.Errorf("session %s: generate successor marker: %w", id, err)
	}

	var (
		credential      hookcapture.Credential
		grant           *hookcapture.Grant
		tempReady       bool
		freshLaunch     bool
		freshAssignedID string
		storePinEnv     []string
	)
	if configured {
		_, freshLaunch = launchShape(source, desc)
		if !freshLaunch {
			pinEnv, storeErr := m.conversationStoreEnv(gctx, desc, source)
			var gone *ConversationStoreGoneError
			if errors.As(storeErr, &gone) {
				// The store is gone BY NAME. Resuming would only ask a
				// different directory about a conversation it never held, so
				// nothing is launched. The identity and the store record stay
				// on the row: the directory may come back with a restored
				// volume, and a forgotten identity would make that
				// conversation unreachable for good. The automatic
				// opportunity closes so the sweep stops asking; the user's
				// card names the gone store and offers a fresh session.
				if closeErr := m.repo.CloseRecoveryOpportunity(gctx, source,
					fmt.Sprintf("the conversation's recorded store %s is gone; the recorded conversation is kept for an explicit resume or a fresh session", gone.Path)); closeErr != nil {
					return nil, errors.Join(storeErr, closeErr)
				}
				return nil, errors.Join(ErrNotResumable, ErrResumeRefusedByProvider, storeErr)
			}
			if storeErr != nil {
				return nil, storeErr
			}
			storePinEnv = pinEnv
		}
		if freshLaunch {
			// The fresh rung takes launch-shaped material, not resume
			// attestation: there is no prior thread to attest, so the
			// attested-provider nil-identity refusal below must not apply.
			// An assigned-identity provider is handed a FRESH uuid — assigned
			// ids are single-use even when their first launch died before the
			// model replied, so a forgotten or refused id is never replayed —
			// and, when the provider tracks identity through its hook, a
			// startup grant so the new conversation is tracked from its first
			// entry. A retrieved provider needs the same grant: without it the
			// new conversation has no recorded identity and the next recovery
			// launches fresh again.
			if desc.IdentityContract() == provider.IdentityAssigned {
				freshAssignedID, err = m.newID()
				if err != nil {
					return nil, fmt.Errorf("session %s: generate fresh assigned identity: %w", id, err)
				}
			}
			if (desc.IdentityContract() == provider.IdentityRetrieved || declaresStructuredHookCapture(desc)) &&
				m.captures != nil && m.captureURLBase != "" {
				preparedCredential, preparedGrant, prepareErr := m.captures.PrepareLaunchGrant(
					source.ID, source.Provider, workDir, source.Generation+1, marker)
				if prepareErr != nil {
					return nil, prepareErr
				}
				credential, grant = preparedCredential, &preparedGrant
			}
		} else if _, attested := resumeAttestationSource(desc); attested {
			if m.captures == nil || m.captureURLBase == "" {
				return nil, ErrNotResumable
			}
			preparedCredential, preparedGrant, prepareErr := m.captures.PrepareResumeGrant(
				source.ID, source.Provider, workDir, source.Generation+1, marker,
				*source.NativeSessionID)
			if prepareErr != nil {
				return nil, prepareErr
			}
			credential, grant = preparedCredential, &preparedGrant
		} else if startupResumeCapture(desc) {
			// omp's session_start fires on a resumed process and carries
			// the resumed id with the same startup-shaped source a fresh
			// launch posts, so the resume takes the same generation-bound
			// startup grant a fresh launch takes. Identity is captured
			// exactly as at launch; a resume-shaped grant would demand a
			// distinction the pinned hook does not emit.
			if m.captures == nil || m.captureURLBase == "" {
				return nil, ErrNotResumable
			}
			preparedCredential, preparedGrant, prepareErr := m.captures.PrepareLaunchGrant(
				source.ID, source.Provider, workDir, source.Generation+1, marker)
			if prepareErr != nil {
				return nil, prepareErr
			}
			credential, grant = preparedCredential, &preparedGrant
		}
	}
	slot, err := m.repo.BeginSuccessorSlot(gctx, source, marker, grant, freshAssignedID)
	if err != nil {
		return nil, err
	}
	failBeforeSpawn := func(stage string, cause error) (*Session, error) {
		// The unwind must outlive the context that aborted the flight. The
		// sweep budget expiring mid-admission is the ordinary way to arrive
		// here, and a rollback run under that same expired context fails at
		// its first database write — leaving the successor slot and the
		// spent opportunity on the row, which excludes the session from
		// every later sweep for good.
		wctx, cancelW := context.WithTimeout(context.WithoutCancel(gctx), recoveryCleanupBound)
		defer cancelW()
		if grant != nil {
			if _, revokeErr := m.captures.RevokeFencedState(wctx, id,
				slot.CandidateGeneration, slot.CandidateIdentityMarker); revokeErr != nil {
				return nil, errors.Join(cause, revokeErr)
			}
		}
		diagnostic := fmt.Sprintf(
			"before_spawn=%s provider=%s source_generation=%d candidate_generation=%d error_class=%s",
			stage, source.Provider, source.Generation, slot.CandidateGeneration,
			recoveryErrorClass(cause))
		var tempErr error
		if tempReady {
			tempErr = m.removeGenerationTemp(id, slot.CandidateGeneration,
				slot.CandidateIdentityMarker)
		}
		// The flight's own context names the abort, not the error: tmux
		// wraps command failures into formatted strings that drop the
		// context chain, so a deadline that surfaced through a tmux call
		// would otherwise read as a deterministic failure and spend the
		// opportunity on nothing.
		if abortedByContext(cause) || gctx.Err() != nil {
			// Nothing was launched, so the provider answered nothing: the
			// attempt returns to the next sweep pass, and the series count
			// alone bounds the retry loop.
			_, returnErr := m.repo.ReturnAbortedRecoveryOpportunity(wctx, slot,
				source.NativeSessionID,
				"automatic recovery aborted before launch: "+diagnostic)
			return nil, errors.Join(ErrNotResumable, cause, returnErr, tempErr)
		}
		_, retireErr := m.repo.RetireSuccessorSlot(wctx, slot, diagnostic)
		return nil, errors.Join(ErrNotResumable, cause, retireErr, tempErr)
	}
	if !configured {
		return failBeforeSpawn("provider_descriptor", errors.New("provider has no recovery descriptor"))
	}
	if err := desc.Availability(); err != nil {
		return failBeforeSpawn("provider_availability", err)
	}
	if err := executableRunnable(desc.Executable()); err != nil {
		return failBeforeSpawn("provider_executable", err)
	}
	// No compatibility or hook gate here: refusing to revive a session because
	// an upstream CLI reworded its --help would destroy a user's work over a
	// measurement that proves nothing about the provider.
	expected, err := expectedExecutable(desc, desc.Executable())
	if err != nil {
		return failBeforeSpawn("provider_executable_identity", err)
	}
	bootEpoch, err := currentBootEpoch()
	if err != nil {
		return failBeforeSpawn("boot_identity", err)
	}
	skipPermissions := false
	if m.settings != nil {
		skipPermissions, err = m.settings.SkipPermissions(gctx)
		if err != nil {
			return failBeforeSpawn("launch_settings", err)
		}
	}
	// The candidate generation's Deck-provisioned hook settings, for providers
	// whose hook registration travels in argv. Named before argv is built,
	// written once the generation directory exists below.
	hookSettingsPath := m.provisionedHookSettingsPath(desc, id,
		slot.CandidateGeneration, slot.CandidateIdentityMarker)
	var argv []string
	if freshLaunch {
		argv, err = desc.NewSessionArgs(provider.LaunchSpec{
			WorkDir: workDir, SkipPermissions: skipPermissions,
			SessionID:        freshAssignedID,
			HookSettingsPath: hookSettingsPath,
		})
	} else {
		argv, err = desc.ResumeSessionArgs(provider.ResumeSpec{
			WorkDir: workDir, NativeSessionID: *source.NativeSessionID,
			SkipPermissions:  skipPermissions,
			HookSettingsPath: hookSettingsPath,
		})
	}
	if err != nil || len(argv) == 0 {
		return failBeforeSpawn("provider_resume_argv",
			errors.Join(errors.New("provider refused documented recovery argv"), err))
	}
	term, err := m.tmux.DefaultTerminal(gctx)
	if err != nil {
		return failBeforeSpawn("tmux_terminal", err)
	}
	if err := m.leases.Invalidate(gctx, id,
		fmt.Sprintf("successor generation %d", slot.CandidateGeneration)); err != nil {
		return failBeforeSpawn("lease_invalidation", err)
	}
	extraEnv := append(sessionIdentityEnv(source.ProjectID, id), m.unconfinedControlEnv(source.ProjectID, id)...)
	extraEnv = append(extraEnv, m.projectPreviewEnvironment()...)
	extraEnv = append(extraEnv, m.harnessLoginEnvironment(gctx, desc)...)
	// The store pin rides the launch's extra environment, which launchArgv
	// gives precedence over both the PermittedEnvironment-inherited
	// value and the descriptor's Environment() additions — the explicit pin
	// must win, or the diverged environment would silently send the resume to
	// the wrong store.
	extraEnv = append(extraEnv, storePinEnv...)
	if grant != nil {
		extraEnv = append(extraEnv,
			"NOISEDECK_SESSION_HOOK_URL="+m.captureURLBase+"/api/hooks/capture",
			"NOISEDECK_SESSION_HOOK_TOKEN="+credential.Token,
		)
		if m.captureSocket != "" {
			extraEnv = append(extraEnv, "NOISEDECK_SESSION_HOOK_SOCKET="+m.captureSocket)
		}
	}
	tempDir, err := m.prepareGenerationTemp(id, slot.CandidateGeneration,
		slot.CandidateIdentityMarker)
	if err != nil {
		return failBeforeSpawn("temporary_directory", err)
	}
	tempReady = tempDir != ""
	if tempReady {
		extraEnv = append(extraEnv, "TMPDIR="+tempDir)
	}
	extraEnv = append(extraEnv, hookDiagnosticsEnvironment(tempDir)...)
	if hookErr := writeProvisionedHookSettings(desc, hookSettingsPath); hookErr != nil {
		return failBeforeSpawn("hook_settings", hookErr)
	}
	// Recovery REPLACES the occupant of the session's own pane; it never opens
	// a second one. A session row is the durable thing the user named and
	// arranged their work around, and the process inside it is a replaceable
	// tenant. Respawning in place keeps the pane, its scrollback, and every
	// attachment, so the dead run's final output stays on screen above the
	// revived one — the same thing a person sees re-running a command in the
	// terminal it died in.
	wrappedArgv := launchArgv(desc, argv, term, extraEnv)
	if desc.Name() == provider.Codex {
		m.recordCodexLaunch(id, slot.CandidateGeneration, wrappedArgv[:len(wrappedArgv)-len(argv)], workDir)
	}
	spec := tmux.SessionSpec{
		WorkDir: workDir, Argv: wrappedArgv,
		Generation: slot.CandidateGeneration, Marker: marker,
		RetainPartialOnError: true,
	}
	var (
		info     *tmux.PaneInfo
		spawnErr error
		// spawnedInfo holds the spawn's own observation for the abort
		// resolver; assigned once the spawn completes below.
		spawnedInfo *tmux.PaneInfo
		name        string
	)
	// The abort resolver: when the caller's SCHEDULE — the sweep budget, a
	// per-recovery gate — ends the flight after the candidate was
	// launched, every intermediate step from stabilization to attestation
	// unwinds with a context error, and unwinding under that same expired
	// context would leave an open successor slot and a spent opportunity:
	// the state that excludes a session from every later sweep. This
	// resolver runs under a context that survives the caller's deadline and
	// decides from observation, not from the error: a candidate still
	// proven ours and alive commits (attestation is confirmation, not
	// permission, and discarding a healthy pane to satisfy a schedule would
	// destroy the work recovery exists to preserve), while a candidate that
	// died records the provider's answer. A candidate that cannot be proven
	// ours at all is left exactly as a crash between the steps would have
	// left it — provisional pane, open slot — for the converger to resolve;
	// no retry is scheduled for it here.
	//
	// A deliberate CANCELLATION — a shutdown, a user abandoning the
	// click, a workspace lock quiescing the cohort — is not a schedule
	// running out, and the resolver declines it: the stopper may want the
	// workspace quiet, and the unresolved live successor is what makes the
	// workspace lock refuse until the converger has resolved the candidate.
	// Pre-spawn there is nothing to keep quiet, so failBeforeSpawn hands a
	// cancelled admission back on its own; everything this resolver sees is
	// post-launch, where name is always assigned.
	defer func() {
		if !abortedByContext(rerr) && gctx.Err() == nil {
			// Not a schedule abort. Like failBeforeSpawn, the flight's own
			// context is the authority: tmux drops the context chain when
			// it wraps command failures.
			return
		}
		if gctx.Err() == context.Canceled {
			return
		}
		wctx, cancelW := context.WithTimeout(context.WithoutCancel(gctx), recoveryCleanupBound)
		defer cancelW()
		open, err := m.repo.SuccessorSlot(wctx, source.ID)
		if err != nil || open == nil {
			// Committed, refused, retired, or already unwound elsewhere.
			return
		}
		observed, observeErr := m.tmux.PaneInfoForSession(wctx, name)
		if observeErr == nil {
			observeErr = verifySuccessorPane(open, name, observed)
			if observeErr != nil {
				// A flight parked between spawning and recording its
				// process tuple leaves the slot nothing to verify against;
				// the spawn's own observation is the proof that flight had.
				observeErr = verifySpawnedCandidate(open, name, observed, spawnedInfo)
			}
		}
		if observeErr != nil {
			// The candidate cannot be proven ours under a surviving
			// context; the converger resolves the provisional pane exactly
			// as a crash between the steps would have.
			return
		}
		if observed.Dead {
			// A pane read after death carries no group or start ticks, so
			// the refusal records the tuple proved while the candidate was
			// alive: the slot's recording when there is one, else the
			// spawn observation. The PID match above is what licenses
			// carrying either over.
			corpse := *observed
			switch {
			case open.CandidateProcessGroupID != nil && *open.CandidateProcessGroupID > 1:
				corpse.ProcessGroupID, corpse.StartTicks =
					*open.CandidateProcessGroupID, *open.CandidateStartTicks
			case spawnedInfo != nil && spawnedInfo.ProcessGroupID > 1:
				corpse.ProcessGroupID, corpse.StartTicks =
					spawnedInfo.ProcessGroupID, spawnedInfo.StartTicks
			}
			if !freshLaunch && corpse.ProcessGroupID > 1 && corpse.StartTicks > 0 {
				rs, rerr = m.recordRefusedResume(wctx, source, open, name,
					expected, bootEpoch, &corpse, spawnErr)
				return
			}
			rs, rerr = m.retireObservedSuccessor(wctx, source, open, &corpse,
				errors.Join(spawnErr, errors.New("successor exited before commit")))
			return
		}
		rs, rerr = m.commitSuccessor(wctx, source, open, name,
			expected, bootEpoch, observed, !freshLaunch)
	}()
	if pane := m.reusableRecoveryPane(gctx, source); pane != nil {
		name, spec.Name = pane.SessionName, pane.SessionName
		info, spawnErr = m.tmux.RespawnPane(gctx, pane.PaneID, spec)
	} else {
		// The pane did not survive — a tmux server restart, a user
		// killing the window. There is nothing left to replace, so recovery
		// builds the row a new home; the session row, and the card the
		// user sees, are still the same ones.
		name, spec.Name = recoverySessionName(m.prefix, id, marker), recoverySessionName(m.prefix, id, marker)
		info, spawnErr = m.tmux.CreateSession(gctx, spec)
	}
	if info != nil && m.lifecycleHooks != nil && m.lifecycleHooks.resumeSpawned != nil {
		m.lifecycleHooks.resumeSpawned(info)
	}
	if spawnErr != nil || info == nil {
		observed, observeErr := m.tmux.PaneInfoForSession(gctx, name)
		if errors.Is(observeErr, tmux.ErrNoSession) {
			return failBeforeSpawn("tmux_spawn", spawnErr)
		}
		if observeErr != nil {
			return nil, errors.Join(spawnErr, observeErr)
		}
		info = observed
	}
	// The spawn's own observation, kept for the abort resolver: a flight
	// parked between spawning and recording the process tuple leaves the
	// slot nothing to verify a corpse against, and this is the proof that
	// flight had.
	spawnedInfo = info
	// Pin the spawn's process identity before any path may adopt this pane.
	// Until this lands the slot proves nothing, which is the honest state: a
	// crash here leaves a candidate nobody may claim rather than one a later
	// occupant could inherit.
	//
	// A candidate that died on launch is pinned too. Its group and start ticks
	// are gone with the process, but tmux keeps the pid that exited, and that
	// is enough to recognize this corpse later and retire its slot. Skipping it
	// would leave the slot unclaimable forever, and a slot outliving its dead
	// candidate blocks every subsequent recovery of the session.
	if info.PID > 1 {
		// Written without the caller's cancellation: the spawn already
		// happened, and a cancel between spawning a process and recording
		// which process it was is exactly how a candidate becomes
		// unclaimable. The write finishes so a restart can still converge.
		recordCtx, cancelRecord := context.WithTimeout(
			context.WithoutCancel(gctx), recoveryCleanupBound)
		err := m.repo.RecordCandidateProcess(recordCtx, slot, info.PID,
			info.ProcessGroupID, info.StartTicks)
		cancelRecord()
		if err != nil {
			return nil, errors.Join(spawnErr, err)
		}
	}
	if !info.Dead {
		stabilized, observeErr := m.tmux.PaneInfoAfterSpawn(gctx, info)
		if observeErr != nil {
			return m.failObservedSuccessor(gctx, source, slot, name, expected,
				bootEpoch, errors.Join(spawnErr, observeErr))
		}
		info = stabilized
	}
	if err := verifySuccessorPane(slot, name, info); err != nil {
		// A mismatched target grants no cleanup authority. The slot remains so
		// the active surface omits the row and a later exact observation may
		// resolve it.
		return nil, errors.Join(spawnErr, err)
	}
	// Attestation is confirmation, not permission. A provider that resumed
	// and stayed silent is indistinguishable from one whose hook is slow,
	// misconfigured, or removed upstream — and the pane in front of the
	// user is alive either way. Killing it to punish a missing callback
	// destroys the very work recovery exists to preserve, so an unattested
	// successor commits like any other live pane and the user reads the
	// truth off their own terminal.
	//
	// The wait also watches the pane. A resumed candidate that dies before
	// it attests is the common shape of a refusal: claude answers an unknown
	// id in about a second, so the pane is alive at the first observation
	// above and gone before its hook could fire. Noticing it here lets the
	// user's click answer with the refusal instead of committing a corpse
	// the reconciler reports seconds later.
	//
	// A fresh candidate never waits: its grant expects the startup source of
	// a conversation whose identity Deck itself assigned, so there is nothing
	// a callback could add before commit — and a timed-out wait would revoke
	// the very grant the new conversation's identity rolls post to.
	// A deferred resume hook also needs its grant after commit: its first
	// possible delivery is the user's first submission, not process launch.
	var attestation hookcapture.GrantState
	if !info.Dead && slot.CaptureTokenHash != nil && !freshLaunch &&
		!resumeHookAfterFirstSubmission(desc) {
		state, died, waitErr := m.waitForResumeAttestation(gctx, slot, name, info)
		if waitErr != nil {
			// A context error here is the caller's schedule, not the
			// provider: the abort resolver installed above re-observes the
			// candidate under a surviving context and commits a pane still
			// proven ours and alive. Any other failure of the wait unwinds
			// on its own.
			return nil, errors.Join(spawnErr, waitErr)
		}
		if died != nil {
			info = died
		}
		attestation = state
	}
	if info.Dead {
		// A resume whose process is already gone did not resume. The provider
		// is the authority on its own sessions, and it has just answered: it
		// did not continue this identity. The dead candidate is still the
		// row's newest generation — its pane holds the provider's own words
		// about why — so it is committed and its exit recorded exactly as
		// the reconciler would have recorded it a second later. That closes
		// the automatic opportunity, because retrying the identical argv
		// unattended only asks the same question again, once per
		// reconciliation interval, for as long as nobody is watching. The
		// identity itself stays
		// recorded: the answer may change once the user has logged in,
		// restored a volume, or fixed whatever the pane shows, and a fresh
		// conversation is the user's explicit choice, never a substitute
		// made here. The decision rests on the process being gone, never on
		// reading what the provider printed.
		if !freshLaunch {
			if info.ProcessGroupID > 1 && info.StartTicks > 0 {
				return m.recordRefusedResume(gctx, source, slot, name, expected,
					bootEpoch, info, spawnErr)
			}
			// A candidate that died before its process could even be read
			// — an executable that fails at exec — carries no tuple a
			// generation can be committed on. The slot retires with the
			// opportunity spent; the row keeps the crash's evidence and its
			// identity, and the user may ask again.
			return m.retireObservedSuccessor(gctx, source, slot, info,
				errors.Join(spawnErr, ErrResumeRefusedByProvider,
					errors.New("successor exited before commit")))
		}
		return m.retireObservedSuccessor(gctx, source, slot, info,
			errors.Join(spawnErr, errors.New("successor exited before commit")))
	}
	if attestation == hookcapture.GrantConsumed {
		refreshed, readErr := m.repo.SuccessorSlot(gctx, id)
		if readErr != nil {
			return nil, readErr
		}
		if refreshed == nil {
			return m.repo.Get(gctx, id)
		}
		slot = refreshed
	}
	return m.commitSuccessor(gctx, source, slot, name, expected, bootEpoch, info, !freshLaunch)
}

// commitSuccessor publishes the observed candidate as the row's running
// generation.
func (m *Manager) commitSuccessor(ctx context.Context, source *Session, slot *SuccessorSlot,
	name, expected, bootEpoch string, info *tmux.PaneInfo, resumed bool) (*Session, error) {
	id := source.ID
	candidate := successorSession(source, slot, m.tmux.SocketPath(), name, expected,
		bootEpoch, info, resumed)
	committed, err := m.repo.CommitSuccessorRunning(ctx, slot, candidate)
	if err != nil || !committed {
		// A missing slot may mean the commit landed and its response was lost.
		// Anything else stays provisional for boot/periodic convergence; no
		// uncertain database result authorizes process cleanup.
		currentSlot, readErr := m.repo.SuccessorSlot(ctx, id)
		if readErr == nil && currentSlot == nil {
			current, getErr := m.repo.Get(ctx, id)
			if getErr == nil && VerifyIdentity(current, info) == nil && current.Status == StatusRunning {
				return current, nil
			}
			return nil, errors.Join(err, readErr, getErr, ErrRecoverySuperseded)
		}
		return nil, errors.Join(err, readErr, ErrRecoveryInProgress)
	}
	if m.lifecycleHooks != nil && m.lifecycleHooks.resumeCommitted != nil {
		m.lifecycleHooks.resumeCommitted()
	}
	m.syncDisplayName(ctx, id)
	return m.repo.Get(ctx, id)
}

// recordRefusedResume commits a resumed candidate that died before it could
// commit, then records its death, so the row carries the attempt the same way
// it carries one the reconciler observed: the generation advanced, the exit
// evidence on it, the pane and its output kept, the identity kept, and the
// automatic opportunity closed by the refusal judgement. The caller receives
// the refusal; no fresh conversation is started in its place.
func (m *Manager) recordRefusedResume(ctx context.Context, source *Session,
	slot *SuccessorSlot, name, expected, bootEpoch string, info *tmux.PaneInfo,
	spawnErr error) (*Session, error) {
	refusal := errors.Join(spawnErr, ErrResumeRefusedByProvider,
		errors.New("successor exited before commit"))
	committed, err := m.commitSuccessor(ctx, source, slot, name, expected, bootEpoch, info, true)
	if err != nil {
		return nil, errors.Join(refusal, err)
	}
	if committed.Status != StatusRunning {
		// The commit answered with a row another observer already moved on;
		// its exit is theirs to record.
		return nil, errors.Join(ErrNotResumable, refusal)
	}
	code, signal := evidence(info)
	if code == nil && signal == nil {
		// No evidence to judge, but the process is gone before it attested:
		// the opportunity closes all the same, or the sweep would replay
		// the resume once per interval until its budget ran out.
		if _, err := m.repo.MarkDeadInterruptedObserved(ctx, committed,
			"resumed candidate is dead but tmux reported no exit status or signal"); err != nil {
			return nil, errors.Join(refusal, err)
		}
		interrupted, err := m.repo.Get(ctx, source.ID)
		if err != nil {
			return nil, errors.Join(refusal, err)
		}
		if err := m.repo.CloseRecoveryOpportunity(ctx, interrupted, refusedResumeAuditDetail); err != nil {
			return nil, errors.Join(refusal, err)
		}
		return nil, errors.Join(ErrNotResumable, refusal)
	}
	if _, err := m.repo.RecordExitObserved(ctx, committed, code, signal, m.now()); err != nil {
		return nil, errors.Join(refusal, err)
	}
	return nil, errors.Join(ErrNotResumable, refusal)
}

func recoveryErrorClass(err error) string {
	switch {
	case errors.Is(err, provider.ErrHookNotProvisioned):
		return "hook_not_provisioned"
	case errors.Is(err, ErrResumeRefusedByProvider):
		return "provider_refused_identity"
	case errors.Is(err, provider.ErrUnavailable):
		return "provider_unavailable"
	case errors.Is(err, ErrProviderNotLaunchable):
		return "provider_not_launchable"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return "lifecycle"
	}
}

func resumeAttestationSource(desc provider.Descriptor) (string, bool) {
	attestor, ok := desc.(provider.ResumeIdentityAttestationDescriptor)
	if !ok || attestor.ExactResumeHookSource() != hookcapture.ExpectedSourceResume {
		return "", false
	}
	captures, ok := desc.(provider.IdentityCaptureDescriptor)
	if !ok {
		return "", false
	}
	for _, mechanism := range captures.IdentityCaptureMechanisms() {
		if mechanism == provider.IdentityCaptureStructuredNativeHook {
			return hookcapture.ExpectedSourceResume, true
		}
	}
	return "", false
}

// startupResumeCapture reports whether a provider's resume flight reports
// identity through the same process-launch callback a fresh launch uses.
// The source string is matched byte-for-byte, the same standard
// resumeAttestationSource holds claude to: Deck never renames a startup
// callback a resume.
func startupResumeCapture(desc provider.Descriptor) bool {
	capturer, ok := desc.(provider.StartupResumeCaptureDescriptor)
	if !ok || capturer.ResumeCaptureHookSource() != hookcapture.ExpectedSourceStartup {
		return false
	}
	captures, ok := desc.(provider.IdentityCaptureDescriptor)
	if !ok {
		return false
	}
	for _, mechanism := range captures.IdentityCaptureMechanisms() {
		if mechanism == provider.IdentityCaptureStructuredNativeHook {
			return true
		}
	}
	return false
}

// resumeHookAfterFirstSubmission only defers a declared structured resume
// hook with known late timing. Unknown timing keeps the existing bounded wait.
func resumeHookAfterFirstSubmission(desc provider.Descriptor) bool {
	if _, ok := resumeAttestationSource(desc); !ok {
		return false
	}
	hooked, ok := desc.(interface {
		IdentityHook() provider.IdentityHook
	})
	return ok && hooked.IdentityHook().Timing == provider.IdentityHookAfterFirstSubmission
}

// waitForResumeAttestation waits for the resumed candidate's grant to settle
// while watching the candidate's pane. It returns the settled grant state, or
// the dead pane observation when the candidate died first — which is the
// provider answering the resume before its hook could attest it.
//
// Every pane read is held to the slot's recorded candidate tuple, the same
// proof the first observation gave: a pane a user reclaimed during the
// wait answers with a different process, and its death is theirs, not the
// resume's. Accepting it would retire the slot on the strength of a
// stranger's exit and then clean up the user's pane as if it were the
// candidate. A mismatch ends the recovery with ErrIdentityMismatch and no
// cleanup authority, exactly as the first observation would.
//
// The pane is read on a slower cadence than the slot: a tmux round trip per
// 25 ms tick would cost more than the wait is worth, and a death is still
// noticed within a fraction of a second.
func (m *Manager) waitForResumeAttestation(ctx context.Context,
	slot *SuccessorSlot, name string, spawned *tmux.PaneInfo) (hookcapture.GrantState, *tmux.PaneInfo, error) {
	deadline := time.NewTimer(m.resumeAttestationBound)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	paneTicker := time.NewTicker(200 * time.Millisecond)
	defer paneTicker.Stop()
	for {
		current, err := m.repo.SuccessorSlot(ctx, slot.SessionID)
		if err != nil {
			return "", nil, err
		}
		if current == nil || current.CaptureTokenHash == nil || current.CaptureState == nil {
			return "", nil, ErrRecoverySuperseded
		}
		switch *current.CaptureState {
		case hookcapture.GrantConsumed, hookcapture.GrantExpired, hookcapture.GrantRevoked:
			return *current.CaptureState, nil, nil
		}
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-ticker.C:
		case <-paneTicker.C:
			// The same observation the first look used, so a death here
			// carries the spawn's process identity exactly as that path's
			// would and retires through the same cleanup.
			observed, observeErr := m.tmux.PaneInfoAfterSpawn(ctx, spawned)
			if observeErr != nil {
				// A pane that cannot be read decides nothing here; the
				// deadline path and the reconciler judge it later.
				continue
			}
			if err := verifySuccessorPane(slot, name, observed); err != nil {
				return "", nil, err
			}
			if observed.Dead {
				return "", m.awaitDeadPaneEvidence(ctx, observed), nil
			}
		case <-deadline.C:
			revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recoveryCleanupBound)
			state, err := m.captures.RevokeFencedState(revokeCtx,
				slot.SessionID, slot.CandidateGeneration, slot.CandidateIdentityMarker)
			cancel()
			return state, nil, err
		}
	}
}

// awaitDeadPaneEvidence gives tmux a moment to reap a pane it has just
// reported dead. pane_dead flips when the pty closes; the exit status and
// signal land when the child is reaped, which can be a tick later — and a
// death read in between carries no evidence, which would record the refusal
// as an interruption nobody judged. The observation's process identity is
// kept; only the evidence is refreshed.
func (m *Manager) awaitDeadPaneEvidence(ctx context.Context, dead *tmux.PaneInfo) *tmux.PaneInfo {
	if dead.ExitStatus != nil || dead.ExitSignal != nil {
		return dead
	}
	for i := 0; i < 10; i++ {
		select {
		case <-ctx.Done():
			return dead
		case <-time.After(100 * time.Millisecond):
		}
		again, err := m.tmux.PaneInfoByID(ctx, dead.PaneID)
		if err != nil || !again.Dead || again.PID != dead.PID {
			return dead
		}
		if again.ExitStatus != nil || again.ExitSignal != nil {
			out := *dead
			out.ExitStatus, out.ExitSignal = again.ExitStatus, again.ExitSignal
			return &out
		}
	}
	return dead
}

// verifySpawnedCandidate proves an observed pane is this recovery's
// candidate when the slot has no recorded process tuple — the flight parked
// between spawning and recording, or died in between. The pane's retained
// PID must equal the spawn's live PID (a user who reclaimed and
// re-exited the pane answers a different one), and the pane's marker options
// must still name the candidate generation.
func verifySpawnedCandidate(slot *SuccessorSlot, name string, observed, spawned *tmux.PaneInfo) error {
	if slot == nil || observed == nil || spawned == nil ||
		observed.SessionName != name || observed.PID <= 1 ||
		observed.PID != spawned.PID ||
		observed.Generation != slot.CandidateGeneration ||
		observed.Marker != slot.CandidateIdentityMarker ||
		observed.LaunchMarker != slot.CandidateIdentityMarker {
		return ErrIdentityMismatch
	}
	if observed.Dead {
		return nil
	}
	if observed.ProcessGroupID <= 1 || observed.StartTicks <= 0 ||
		observed.ProcessGroupID != spawned.ProcessGroupID ||
		observed.StartTicks != spawned.StartTicks {
		return ErrIdentityMismatch
	}
	return nil
}

// verifySuccessorPane proves a pane holds the candidate this recovery
// launched.
//
// The marker pair and generation are necessary but NOT sufficient. They live
// in pane options and the session environment, both of which survive an
// out-of-band `tmux respawn-pane`, so a user who reclaimed the candidate's
// pane answers to all three exactly. Recovery replaces the occupant of the
// session's own pane, which is a pane a user can see and reach.
// Adopting their process here would write their pid into the row, and every
// later identity check would then defend the wrong process all the way to
// kill(-pgid) — a destructive act with flawless proof of the wrong thing.
//
// The slot's recorded process tuple is what closes that, because pid, group,
// and start ticks are the only members that move when the occupant changes. A
// slot that has not recorded one yet proves nothing and is never adopted.
//
// The executable path is deliberately not compared: it identifies a build, not
// a process, and a runtime upgraded between generations is still this recovery.
func verifySuccessorPane(slot *SuccessorSlot, name string,
	info *tmux.PaneInfo) error {
	if slot == nil || info == nil || info.SessionName != name ||
		info.Generation != slot.CandidateGeneration ||
		info.Marker != slot.CandidateIdentityMarker ||
		info.LaunchMarker != slot.CandidateIdentityMarker {
		return ErrIdentityMismatch
	}
	// Tmux retains the pid that exited, so it is comparable either way; start
	// ticks and the process group are live-only and cannot be revalidated
	// after death.
	if slot.CandidatePanePID == nil || info.PID != *slot.CandidatePanePID {
		return ErrIdentityMismatch
	}
	if info.Dead {
		return nil
	}
	if info.PID <= 1 || info.ProcessGroupID <= 1 || info.StartTicks <= 0 {
		return ErrIdentityMismatch
	}
	if slot.CandidateStartTicks == nil || info.StartTicks != *slot.CandidateStartTicks ||
		slot.CandidateProcessGroupID == nil || info.ProcessGroupID != *slot.CandidateProcessGroupID {
		return ErrIdentityMismatch
	}
	return nil
}

func successorSession(source *Session, slot *SuccessorSlot, socketPath, name, expected,
	bootEpoch string, info *tmux.PaneInfo, resumed bool) *Session {
	candidate := *source
	candidate.TmuxSocketPath = socketPath
	candidate.TmuxSessionName = name
	candidate.TmuxSessionID = &info.SessionID
	candidate.TmuxPaneID = &info.PaneID
	candidate.Generation = slot.CandidateGeneration
	candidate.IdentityMarker = &slot.CandidateIdentityMarker
	candidate.PanePID = &info.PID
	candidate.ProcessGroupID = &info.ProcessGroupID
	candidate.StartTicks = &info.StartTicks
	candidate.ExpectedExecutable = &expected
	candidate.BootEpoch = &bootEpoch
	// Set explicitly: copying source would inherit the PREVIOUS generation's
	// shape, and the refusal window reads this to decide whether the provider
	// was handed an identity at all.
	candidate.GenerationResumed = resumed
	return &candidate
}

func (m *Manager) failObservedSuccessor(ctx context.Context, source *Session,
	slot *SuccessorSlot, name, expected, bootEpoch string, cause error) (*Session, error) {
	info, err := m.tmux.PaneInfoForSession(ctx, name)
	if errors.Is(err, tmux.ErrNoSession) || errors.Is(err, tmux.ErrNoServer) {
		if slot.CaptureTokenHash != nil {
			_, _ = m.captures.RevokeFencedState(ctx, slot.SessionID,
				slot.CandidateGeneration, slot.CandidateIdentityMarker)
		}
		_, retireErr := m.repo.RetireSuccessorSlot(ctx, slot,
			retiredSuccessorDiagnostic("candidate_absent", source, slot, cause))
		return nil, errors.Join(ErrNotResumable, cause, retireErr)
	}
	if err != nil || verifySuccessorPane(slot, name, info) != nil {
		return nil, errors.Join(cause, err, ErrIdentityMismatch)
	}
	return m.retireObservedSuccessor(ctx, source, slot, info, cause)
}

// conversationStoreEnv decides how an exact resume relates to the recorded
// conversation store. Nil-nil means launch as the environment resolves: the
// provider declares no store semantics, the row predates store tracking, or
// the recorded root IS the current resolution. A recorded root that still
// exists but is not what the environment would resolve returns the pin
// assignments (audited); a recorded root that is gone from disk returns
// ConversationStoreGoneError so the caller can name the condition on the
// card without launching anything.
//
// An unresolvable CURRENT root deliberately decides nothing: pinning or
// refusing on a resolution failure would gate the resume on evidence that
// does not exist, and the launch will answer for itself.
func (m *Manager) conversationStoreEnv(ctx context.Context, desc provider.Descriptor,
	source *Session) ([]string, error) {
	pinning, ok := desc.(provider.StorePinningDescriptor)
	if !ok || source.NativeStoreRoot == nil || *source.NativeStoreRoot == "" {
		return nil, nil
	}
	recorded := *source.NativeStoreRoot
	current, err := pinning.CurrentStoreRoot()
	if errors.Is(err, provider.ErrStoreRootUnresolvable) {
		// An environment that names no store offers no evidence of
		// divergence: pinning or refusing on it would gate the resume on a
		// fact that does not exist, so the launch proceeds untouched and
		// answers for itself.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if recorded == current {
		return nil, nil
	}
	if storeDirectoryGone(recorded) {
		return nil, &ConversationStoreGoneError{Path: recorded}
	}
	if err := m.repo.AppendEvent(ctx, source.ID, AuditResumeAttempt,
		fmt.Sprintf("resume pinned to the conversation's recorded store %s (current resolution %s)",
			recorded, current)); err != nil {
		return nil, err
	}
	return pinning.StorePinEnvironment(recorded), nil
}

// recordedStoreGone reports whether the conversation's recorded store is gone
// by name: a store-pinning provider, a recorded root that is not what the
// environment resolves today, and no directory at that root. It is the
// presentation's view of the same fact conversationStoreEnv refuses on, so
// the card and the refusal agree.
func recordedStoreGone(desc provider.Descriptor, s *Session) bool {
	pinning, ok := desc.(provider.StorePinningDescriptor)
	if !ok || s == nil || s.NativeStoreRoot == nil || *s.NativeStoreRoot == "" {
		return false
	}
	current, err := pinning.CurrentStoreRoot()
	if err != nil || current == *s.NativeStoreRoot {
		return false
	}
	return storeDirectoryGone(*s.NativeStoreRoot)
}

// storeDirectoryGone is a content-blind existence check on a store root; it
// reads nothing inside it.
func storeDirectoryGone(root string) bool {
	info, err := os.Stat(root)
	return err != nil || !info.IsDir()
}

// retiredSuccessorDiagnostic is the structured detail the retirement audit
// carries. It names the stage, the provider, the generations, and the error
// class — content-free facts a user can act on — never provider output.
func retiredSuccessorDiagnostic(stage string, source *Session, slot *SuccessorSlot,
	cause error) string {
	return fmt.Sprintf(
		"after_spawn=%s provider=%s source_generation=%d candidate_generation=%d error_class=%s",
		stage, source.Provider, slot.SourceGeneration, slot.CandidateGeneration,
		recoveryErrorClass(cause))
}

func (m *Manager) retireObservedSuccessor(ctx context.Context, source *Session,
	slot *SuccessorSlot, info *tmux.PaneInfo, cause error) (*Session, error) {
	if slot.CaptureTokenHash != nil {
		if _, err := m.captures.RevokeFencedState(ctx, slot.SessionID,
			slot.CandidateGeneration, slot.CandidateIdentityMarker); err != nil {
			return nil, errors.Join(cause, err)
		}
	}
	if err := m.discardUncommittedResume(ctx, source, info); err != nil {
		return nil, errors.Join(cause, err)
	}
	stage := "candidate_discarded"
	if errors.Is(cause, ErrResumeRefusedByProvider) {
		stage = "provider_refused_identity"
	}
	_, err := m.repo.RetireSuccessorSlot(ctx, slot,
		retiredSuccessorDiagnostic(stage, source, slot, cause))
	return nil, errors.Join(ErrNotResumable, cause, err)
}

// discardUncommittedResume signals or removes only the exact observed
// successor tuple. A name, provider identity, or absent pane never authorizes
// a process effect.
func (m *Manager) discardUncommittedResume(ctx context.Context, s *Session,
	spawned *tmux.PaneInfo) error {
	if spawned == nil {
		return nil
	}
	current, err := m.tmux.PaneInfoByID(ctx, spawned.PaneID)
	switch {
	case err == nil:
		if current.SessionID != spawned.SessionID || current.SessionName != spawned.SessionName ||
			current.PaneID != spawned.PaneID || current.Generation != spawned.Generation ||
			current.Marker != spawned.Marker || current.LaunchMarker != spawned.LaunchMarker {
			return fmt.Errorf("candidate pane identity changed before cleanup: %w", ErrIdentityMismatch)
		}
		if !current.Dead && (current.PID != spawned.PID ||
			current.ProcessGroupID != spawned.ProcessGroupID ||
			current.StartTicks != spawned.StartTicks ||
			(spawned.Executable != "" && current.Executable != spawned.Executable)) {
			return fmt.Errorf("candidate process identity changed before cleanup: %w", ErrIdentityMismatch)
		}
	case errors.Is(err, tmux.ErrNoSession):
		current = nil
	case errors.Is(err, tmux.ErrNoServer):
		return fmt.Errorf("observe candidate pane: %w", err)
	default:
		return fmt.Errorf("observe candidate pane: %w", err)
	}

	pgid := spawned.ProcessGroupID
	if current != nil && current.Dead {
		if current.PID <= 1 || (spawned.PID > 1 && current.PID != spawned.PID) ||
			(pgid > 1 && pgid != current.PID) {
			return ErrUnverifiable
		}
		pgid = current.PID
	}
	if current != nil && !current.Dead {
		if current.PID <= 1 || current.StartTicks <= 0 || pgid <= 1 {
			return ErrUnverifiable
		}
		alive, err := tmux.ProcAlive(current.PID, current.StartTicks)
		if err != nil {
			return fmt.Errorf("verify candidate process: %w", err)
		}
		if !alive {
			current.Dead = true
		} else {
			observedPGID, err := m.procGroup(current.PID, current.StartTicks)
			if err != nil || observedPGID != pgid {
				return errors.Join(ErrIdentityMismatch, err)
			}
			if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil &&
				!errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("signal candidate process group: %w", err)
			}
			if err := m.repo.AppendEvent(ctx, s.ID, AuditResumeAttempt,
				fmt.Sprintf("discarded uncommitted recovery generation %d", spawned.Generation)); err != nil {
				return err
			}
		}
	}
	if pgid <= 1 {
		return ErrUnverifiable
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		members, err := tmux.GroupMembers(pgid)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			break
		}
		if current == nil || current.Dead {
			return ErrUnverifiable
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	if current == nil {
		return nil
	}
	current, err = m.tmux.PaneInfoByID(ctx, spawned.PaneID)
	if errors.Is(err, tmux.ErrNoSession) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.SessionID != spawned.SessionID || current.PaneID != spawned.PaneID ||
		current.Generation != spawned.Generation || current.Marker != spawned.Marker ||
		current.LaunchMarker != spawned.LaunchMarker {
		return ErrIdentityMismatch
	}
	applied, err := m.tmux.KillPaneIfIdentity(ctx, tmux.PaneIdentity{
		SessionID: current.SessionID, PaneID: current.PaneID, PanePID: current.PID,
		Generation: current.Generation, Marker: current.Marker,
		LaunchMarker: current.LaunchMarker,
	})
	if errors.Is(err, tmux.ErrNoSession) {
		return nil
	}
	if err != nil {
		return err
	}
	if !applied {
		return ErrIdentityMismatch
	}
	return nil
}

func (m *Manager) ConvergeRecoveryCandidates(ctx context.Context) error {
	done, err := m.gate.Admit()
	if errors.Is(err, inputgate.ErrWorkspaceLocking) {
		return nil
	}
	if err != nil {
		return err
	}
	defer done()
	if err := m.convergeLegacyRecoveryCandidates(ctx); err != nil {
		return err
	}
	slots, err := m.repo.ListSuccessorSlots(ctx)
	if err != nil {
		return err
	}
	for _, slot := range slots {
		gate := m.gate.Session(slot.SessionID)
		gate.Lock()
		err := m.convergeSuccessorSlot(ctx, slot)
		gate.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// findCandidatePane locates a provisional successor by its candidate marker
// and reports the tmux session name it was found under. A candidate that no
// pane claims is reported with tmux.ErrNoSession, which is the caller's
// signal that the launch left nothing behind to converge.
func (m *Manager) findCandidatePane(ctx context.Context,
	slot *SuccessorSlot) (string, *tmux.PaneInfo, error) {
	panes, err := m.tmux.ListPanes(ctx)
	if err != nil {
		return "", nil, err
	}
	var found *tmux.PaneRecord
	for i, pane := range panes {
		if pane.Marker != slot.CandidateIdentityMarker ||
			pane.Generation != slot.CandidateGeneration {
			continue
		}
		// Two panes answering to one candidate marker is a contradiction, and
		// this result decides cleanup authority. Naming either one would be a
		// guess, so name neither. This is deliberately NOT the absent case:
		// absence retires the slot and revokes the grant, and ambiguity must
		// never buy that authority. The slot stays for a later unambiguous
		// observation.
		if found != nil {
			return "", nil, errAmbiguousCandidate
		}
		found = &panes[i]
	}
	if found == nil {
		return "", nil, tmux.ErrNoSession
	}
	info, err := m.tmux.PaneInfoByID(ctx, found.PaneID)
	if err != nil {
		return "", nil, err
	}
	return found.SessionName, info, nil
}

func (m *Manager) convergeSuccessorSlot(ctx context.Context, slot *SuccessorSlot) error {
	source, err := m.repo.Get(ctx, slot.SessionID)
	if err != nil {
		return err
	}
	desc, ok := m.providers[provider.Name(source.Provider)]
	if !ok {
		return nil
	}
	expected, expectedOK := recoveryExpectedExecutable(desc)
	if !expectedOK {
		return nil
	}
	// Rediscovery follows the MARKER, not a derived tmux name. Recovery
	// replaces the occupant of the row's own pane whenever that pane survived,
	// so the candidate answers to whatever name the session already had; only
	// a recovery that had to build a new home carries the derived one. The
	// marker is unique to this candidate generation either way.
	name, info, err := m.findCandidatePane(ctx, slot)
	if errors.Is(err, errAmbiguousCandidate) {
		return nil
	}
	if errors.Is(err, tmux.ErrNoSession) || errors.Is(err, tmux.ErrNoServer) {
		if slot.CaptureTokenHash != nil && slot.CaptureState != nil &&
			*slot.CaptureState == hookcapture.GrantPending {
			if _, err := m.captures.RevokeFencedState(ctx, slot.SessionID,
				slot.CandidateGeneration, slot.CandidateIdentityMarker); err != nil {
				return err
			}
		}
		_, err = m.repo.RetireSuccessorSlot(ctx, slot, "")
		return err
	}
	if err != nil {
		return err
	}
	paneIsExact := verifySuccessorPane(slot, name, info) == nil
	if !paneIsExact {
		return nil
	}
	if source.RemovedAt != nil || info.Dead ||
		(slot.CaptureState != nil && (*slot.CaptureState == hookcapture.GrantExpired ||
			*slot.CaptureState == hookcapture.GrantRevoked)) {
		if slot.CaptureTokenHash != nil && slot.CaptureState != nil &&
			*slot.CaptureState == hookcapture.GrantPending {
			if _, err := m.captures.RevokeFencedState(ctx, slot.SessionID,
				slot.CandidateGeneration, slot.CandidateIdentityMarker); err != nil {
				return err
			}
		}
		if err := m.discardUncommittedResume(ctx, source, info); err != nil {
			return err
		}
		_, err = m.repo.RetireSuccessorSlot(ctx, slot, "")
		return err
	}
	if slot.CaptureTokenHash != nil {
		if slot.CaptureState == nil {
			return errors.New("session: successor capture grant is missing")
		}
		if *slot.CaptureState == hookcapture.GrantPending && !resumeHookAfterFirstSubmission(desc) {
			return nil
		}
		if *slot.CaptureState != hookcapture.GrantConsumed && *slot.CaptureState != hookcapture.GrantPending {
			return nil
		}
	}
	bootEpoch, err := currentBootEpoch()
	if err != nil {
		return err
	}
	_, convergedFresh := launchShape(source, desc)
	candidate := successorSession(source, slot, m.tmux.SocketPath(), name, expected,
		bootEpoch, info, !convergedFresh)
	_, err = m.repo.CommitSuccessorRunning(ctx, slot, candidate)
	return err
}

func (m *Manager) convergeLegacyRecoveryCandidates(ctx context.Context) error {
	attempts, err := m.repo.ListLegacyRecoveryAttempts(ctx)
	if err != nil {
		return err
	}
	for _, attempt := range attempts {
		if attempt.State == "starting" {
			if err := m.repo.FinishLegacyRecoveryAttempt(ctx, attempt, "failed"); err != nil {
				return err
			}
			continue
		}
		if attempt.TmuxSocketPath != m.tmux.SocketPath() {
			if err := m.repo.FinishLegacyRecoveryAttempt(ctx, attempt, "uncertain"); err != nil {
				return err
			}
			continue
		}
		info, observeErr := m.tmux.PaneInfoForSession(ctx, attempt.TmuxSessionName)
		if errors.Is(observeErr, tmux.ErrNoSession) || errors.Is(observeErr, tmux.ErrNoServer) {
			if err := m.repo.FinishLegacyRecoveryAttempt(ctx, attempt, "failed"); err != nil {
				return err
			}
			continue
		}
		if observeErr != nil {
			return observeErr
		}
		bootEpoch, bootErr := currentBootEpoch()
		exact := bootErr == nil && bootEpoch == attempt.BootEpoch &&
			info.SessionName == attempt.TmuxSessionName &&
			info.Generation == attempt.CandidateGeneration &&
			info.Marker == attempt.CandidateIdentityMarker &&
			info.LaunchMarker == attempt.CandidateIdentityMarker &&
			(info.Dead || (info.Executable == attempt.ExpectedExecutable &&
				info.PID > 1 && info.ProcessGroupID > 1 && info.StartTicks > 0))
		if !exact {
			if err := m.repo.FinishLegacyRecoveryAttempt(ctx, attempt, "uncertain"); err != nil {
				return err
			}
			continue
		}
		source, err := m.repo.Get(ctx, attempt.SessionID)
		if err != nil {
			return err
		}
		if err := m.discardUncommittedResume(ctx, source, info); err != nil {
			return err
		}
		if err := m.repo.FinishLegacyRecoveryAttempt(ctx, attempt, "failed"); err != nil {
			return err
		}
	}
	return m.repo.RecordLegacyRecoveryReceipt(ctx)
}

// recoveryProbeSessionID is a syntactically valid assigned-identity value for
// capability probes only. It is never launched: every real fresh candidate
// mints its own uuid, because assigned ids are single-use.
const recoveryProbeSessionID = "00000000-0000-4000-8000-000000000000"

// Recover is the user's explicit retry: the exact resume of the recorded
// identity, or a fresh conversation only when the row carries no identity to
// resume. The outcome names which of those committed, so a relaunch is
// reported as a relaunch and never implies the previous thread survived.
//
// A refused resume is answered, not worked around. The provider has said it
// did not continue the recorded conversation; the record, its identity, and
// the pane's output stay exactly as they are, and starting a fresh
// conversation is a separate, explicit choice on the card. Deck relaunching
// fresh on its own here would replace the user's conversation on the
// strength of a one-second process exit.
//
// The row is replaced in place: the user gets the card they already have,
// carrying the dead run's output above the revived one.
func (m *Manager) Recover(ctx context.Context, id string) (*Session, RecoveryOutcome, error) {
	done, err := m.gate.Admit()
	if err != nil {
		return nil, "", err
	}
	defer done()
	source, err := m.repo.Get(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if !terminalRecoverySource(source) {
		return nil, "", ErrNotResumable
	}
	if _, err := m.repo.ReopenRecoveryOpportunity(ctx, source); err != nil {
		return nil, "", err
	}
	revived, err := m.recoverAdmitted(ctx, id, "", true)
	if err != nil {
		return nil, "", err
	}
	return revived, committedRecoveryOutcome(revived), nil
}

// committedRecoveryOutcome derives what a committed recovery did from the
// generation's own record, never from intent: only a generation launched by
// resuming the recorded identity may be called a resume.
func committedRecoveryOutcome(s *Session) RecoveryOutcome {
	if s != nil && s.GenerationResumed {
		return RecoveryOutcomeResumed
	}
	return RecoveryOutcomeRelaunched
}
