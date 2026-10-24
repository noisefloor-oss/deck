# CI checks

## Private runner routing

All Linux Fast checks jobs, including the `fast-checks` aggregate, use
`self-hosted`, `Linux`, `ARM64` runners. Jobs queue when no matching runner
is online; there is no automatic GitHub-hosted fallback. Private runner-group
access is restricted to the selected repositories. Public mirrors require
separate organization-owned runners; they never share the private pool.

Private repositories disable fork PR workflow execution in GitHub's Actions
policy, outside the candidate workflow's control. Public mirrors require
maintainer workflow approval for all external contributors and an isolated
public runner pool; never approve fork workflow execution on a private pool.
The YAML rejection step is defense in depth, not an isolation boundary.
Repository code from fork pull requests is not automatically admitted.
A metadata-only rejection step fails required checks before checkout; import
the change to a same-repository branch to run CI. The aggregate always runs
and retains its result validation for admitted changes. Native
Linux/AMD64, macOS and Windows release jobs retain explicit OS/architecture
labels; ARM64 checks do not certify another architecture. Hosted-only disk
cleanup is disabled on self-hosted machines.

Review and required-check protections are unchanged. Old PR heads must
acquire fresh checks from the updated workflow
before merging; rerunning an old head's hosted workflow is not a migration.

Private same-repository pull requests run only the lightweight preflight:
selector unit tests, public archive/Markdown validation and the required
aggregate. They do not run regression suites or call Jev. Approval lets the
PR enter the merge queue; it does not certify the application tests.

Private merge-queue runs use context-rich Jev selection by default. Each
suite is selected at an impact probability
of at least 50%; documentation remains mandatory. UI, gateway and project-app
preview browser lanes are assessed separately. These are model estimates,
not calibrated guarantees. Full manual Fast checks and release acceptance
are unchanged.

The credentialed CI advisor runs from trusted main when merge-queue Fast
checks is requested, without checking out or executing candidate code. Its
planner refuses PR runs, including manual requests for those runs. It reads the
complete bounded patch and the actual suite implementations as data, then
publishes a small credential-free artifact retained for one day. The consumer
checks the publisher workflow and binds the artifact to the exact repository,
run, event, base and head. It also recomputes the selections from the validated
probabilities. API credentials never enter PR jobs.

The merge-queue scope job waits up to one minute for that plan. Missing credentials, API
failure, stale or malformed plans, unsupported or oversized patches and
unknown queue layouts use the Python path map below. The fallback uses the
complete local merge-base-to-head Git diff, not the last commit or a truncated
API list. Public repositories keep the Python selector. Private fork workflows remain
disabled by GitHub policy; public forks require an isolated public pool. The workflow summary names the selector and selected checks.

| Changed area | Python fallback checks |
| --- | --- |
| README, contributing, changelog, security documentation; Markdown under docs or skills | Public source archive contents and relative Markdown links |
| Frontend source, assets, tooling and probes | Documentation, frontend typecheck/tests/build, three UI browser lanes |
| Go source under cmd or internal | Documentation, Go formatting, lint/vulnerability checks, parallel and serialized Go tests, experiment tests |
| Multiple recognized areas | Union of their suites |
| Authentication, session, transport and other backend boundary code | Go checks plus frontend and all browser lanes |
| CI, deployment scripts and working agreements | Documentation and deployment contract tests; `.go` changes also run Go checks; repair/jail implementation also runs Go and hostile jail checks |
| Workflow definitions under `.github/workflows/` | Full suite |
| Executable policy documents | Go and deployment contract tests that consume those files |
| Public export implementation | Documentation, deployment contracts and extracted-source validation; `.go` changes also run Go checks |
| Shared build files, Go dependencies, or unrecognized paths | Full suite |

A plan chosen by these rules as full validation stays full: the model-chosen plan cannot narrow it.

This is a conservative path map, not semantic dependency analysis. The map and
its tests live in `.github/scripts/ci_scope.py` and `test_ci_scope.py`. A rename
checks both old and new paths. Missing history, malformed events, ambiguous
merge bases, empty diffs, symlinks and executable-mode changes use full checks.

Backend boundary changes retain all browser lanes rather than relying on Go
unit tests alone. Browser cryptography, API contracts, preview/onboarding
probes and frontend dependency-lock changes also retain gateway and preview
lanes. Ordinary UI changes do not rebuild those unrelated Go fixtures.
Extracted-source validation and the hostile repair jail are no longer charged
to every UI or backend change; they run when their own area changes and in
full manual validation.

Merge-queue runs use the complete queue base-to-head diff, including every
change in a grouped batch. If a selected queue test fails, the PR does not
merge: inspect and fix the failure, then enqueue it again. Running regression
tests only at that revision avoids repeating the same suites before and
after queueing. Fork contributions must first be imported to a same-repository branch;
this private-repository policy assumes changes land through the merge queue.
Do not use a private PR preflight as evidence for a direct merge or push.

Manual runs always select the full suite. Fast checks does not
run again on a main push: the merge queue has already tested the exact commit
that lands. Installations that allow direct pushes to main must explicitly
dispatch Fast checks to validate those commits. Release Acceptance, image
publication, schedules and independent review are unchanged.

Private fork workflows are disabled by GitHub policy. Public fork execution
requires an isolated public pool; persistent CI requires importing the change
to a same-repository branch, which receives the ordinary required checks.

The required `fast-checks` gate always runs. It fails if scope selection fails,
if a selected job fails, is cancelled or skips unexpectedly, or if an
unselected job runs. An intentional skip is not a successful test: the gate checks the
plan against every actual job result. It permits selective merge-queue plans,
but still requires a full plan for manual, unknown and fork events.

The archive-content and Markdown checks run once per workflow, including
documentation-only changes. Full exported-source build, vet and test lanes
remain mandatory in the full suite. All test invocations retain `-count=1`.

## Jev credentials and repair advice

`TYPESAFE_API_KEY` stays in the existing Actions repository secret and is
received only by trusted-main code on the workflow-restricted
`deck-trusted-review` runner group. The planner never checks out candidate
source; the group admits only its explicitly listed main-branch workflows. The advisor
is not itself a required check: its absence selects the Python fallback,
not a green exemption from tests. The existing repair workflow's Jev failure
advice remains separate and does not acquire new repair authority.

The API summary reports the model, each probability, selected suites and
browser lanes, latency, token usage and estimated cost. It never prints
patches, source context, credentials or raw API response bodies.
