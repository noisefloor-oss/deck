package harnessbump

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// BranchPrefix is where the job's branches live.
const BranchPrefix = "harness-bump/"

// GH runs the gh CLI with arguments and returns its stdout.
type GH func(ctx context.Context, args ...string) ([]byte, error)

// ExecGH runs the installed gh. On failure the error carries what gh
// said, so a run summary names the cause rather than an exit status:
// its stderr as Output already keeps it, bounded, and when gh put its
// complaint on stdout instead, a bounded head of that.
func ExecGH(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "gh", args...).Output()
	if err != nil {
		said := ""
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			said = strings.TrimSpace(string(exitErr.Stderr))
		}
		if said == "" {
			said = strings.TrimSpace(string(out))
		}
		if len(said) > maxSaid {
			said = said[:maxSaid]
		}
		return out, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, said)
	}
	return out, nil
}

// maxSaid bounds how much of gh's complaint an error carries.
const maxSaid = 2000

// Action is what settling a pull request decided to do.
type Action string

const (
	// ActionReview: no verdict stands at the head; the reviewer runs.
	ActionReview Action = "review"
	// ActionWait: approved, but a check is still running or the state is
	// not yet clean.
	ActionWait Action = "wait"
	// ActionUpdateBranch: approved, but main moved; the branch is brought up
	// to date, which dismisses the verdict and starts the round again.
	ActionUpdateBranch Action = "update-branch"
	// ActionMerge: approved at the head, the gate is green, the state is
	// clean.
	ActionMerge Action = "merge"
	// ActionQueued is an accepted or observed queue request, not a merge.
	ActionQueued Action = "queued"
	// ActionChangesRequested: the verdict stands and says no; a person or an
	// agent takes it from here.
	ActionChangesRequested Action = "changes-requested"
	// ActionBlocked: the gate failed at the head; nothing moves.
	ActionBlocked Action = "blocked"
	// ActionIgnored: the pull request is not one the job made — a fork, a
	// title or branch that is not a catalogue bump, or files beyond the
	// bump's own — so it is neither reviewed nor merged here.
	ActionIgnored Action = "ignored"
	// ActionHandOff: the head is a bump the job made and then someone
	// changed. Whoever changed it may have been a model; the reviewer here
	// is one model and the SDLC wants another, so the review and the merge
	// are theirs to dispatch, not this job's.
	ActionHandOff Action = "hand-off"
)

// Deterministic reports whether the pull request's head is byte-for-byte
// the bump the job made: its recorded target version,
// applied to the head's own base on main. Only such a head is reviewed
// and merged automatically; the review workflow's model is chosen for
// the job's own commits, not for a person's or an agent's. Exact-head build
// evidence establishes publication; a moving latest tag is not ownership.
type Deterministic func(ctx context.Context, harness Harness, version, head string) (bool, error)

// Evidence reports whether a trusted, completed run of the bump workflow
// on main recorded this head after successful per-harness gates: the pin
// tests, the image build and the contract probe. A shape alone is not that
// proof; the workflow's own artifact is.
type Evidence func(ctx context.Context, harness Harness, version, head string) (bool, error)

// Decision is the settlement of one pull request.
type Decision struct {
	Number  int    `json:"number"`
	Branch  string `json:"branch"`
	Head    string `json:"head"`
	Title   string `json:"title"`
	Action  Action `json:"action"`
	Reason  string `json:"reason"`
	Applied bool   `json:"applied"`
}

type pullRequest struct {
	Number            int    `json:"number"`
	State             string `json:"state"`
	Title             string `json:"title"`
	HeadRefName       string `json:"headRefName"`
	HeadRefOid        string `json:"headRefOid"`
	BaseRefName       string `json:"baseRefName"`
	IsCrossRepository bool   `json:"isCrossRepository"`
	Author            struct {
		Login string `json:"login"`
	} `json:"author"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
	Files            []struct {
		Path string `json:"path"`
	} `json:"files"`
	Reviews []struct {
		Body   string `json:"body"`
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		State  string `json:"state"`
		Commit struct {
			Oid string `json:"oid"`
		} `json:"commit"`
	} `json:"reviews"`
	StatusCheckRollup []struct {
		Name       string `json:"name"`
		Context    string `json:"context"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		State      string `json:"state"`
	} `json:"statusCheckRollup"`
}

// Gate is the required status check main's ruleset names.
const Gate = "fast-checks"

const pullRequestFields = "number,state,title,headRefName,headRefOid,baseRefName,isCrossRepository,author,mergeable,mergeStateStatus,reviews,statusCheckRollup,files"

// Settle decides, for every open pull request on a job branch, whether it
// merges, waits, needs a review, or is left alone, and with apply performs
// the merge or branch update. The reviewer's verdict counts only at the
// pull request's current head: a verdict on an older commit is no verdict.
func Settle(ctx context.Context, gh GH, repo, botLogin string, only int, apply bool, deterministic Deterministic, evidence Evidence) ([]Decision, error) {
	raw, err := gh(ctx, "pr", "list", "--repo", repo, "--state", "open", "--limit", "100",
		"--json", pullRequestFields)
	if err != nil {
		return nil, err
	}
	var prs []pullRequest
	if err := json.Unmarshal(raw, &prs); err != nil {
		return nil, fmt.Errorf("harnessbump: gh pr list: %w", err)
	}
	// The job opens its pull requests with the workflow's own token, so
	// their author is that token's login or the Actions bot; a pull
	// request anyone else opened is not the job's, whatever its shape.
	// The Actions token itself cannot ask who it is (the user endpoint
	// answers 403); then the bot is the only author the job can have.
	authors := map[string]bool{"github-actions": true, "github-actions[bot]": true, "app/github-actions": true}
	if self, err := gh(ctx, "api", "user", "--jq", ".login"); err == nil {
		if login := strings.TrimSpace(string(self)); login != "" {
			authors[login] = true
		}
	}
	var decisions []Decision
	var failures []error
	for _, pr := range prs {
		if !strings.HasPrefix(pr.HeadRefName, BranchPrefix) || (only != 0 && pr.Number != only) {
			continue
		}
		d := decide(pr, botLogin, authors)
		recoverCI := !hasFastCheck(pr) && (d.Action == ActionReview || d.Action == ActionWait)
		if d.Action == ActionReview || d.Action == ActionMerge || d.Action == ActionUpdateBranch || recoverCI {
			reason, err := jobHeadProof(ctx, pr, deterministic, evidence)
			if err != nil {
				return decisions, err
			}
			if reason != "" {
				d.Action, d.Reason = ActionHandOff, reason
			}
		}
		var queueBase string
		if d.Action == ActionMerge || d.Action == ActionUpdateBranch {
			d, queueBase, err = prepareQueueDecision(ctx, gh, repo, botLogin, pr, d)
			if err != nil {
				return decisions, err
			}
		}
		// A merge or a branch update also waits on the conversation: an
		// unresolved review thread is the SDLC's own gate, whatever the
		// merge state says.
		if d.Action == ActionMerge || d.Action == ActionUpdateBranch {
			open, err := unresolvedThreads(ctx, gh, repo, pr.Number)
			if err != nil {
				return decisions, err
			}
			if open > 0 {
				d.Action, d.Reason = ActionWait, fmt.Sprintf("%d review thread(s) unresolved", open)
			}
		}
		if apply {
			var err error
			switch d.Action {
			// Every write names the head the decision inspected, so a push
			// between the read and the write fails the write instead of
			// acting on a commit nobody judged.
			case ActionMerge:
				var submitted bool
				submitted, err = enqueueBump(ctx, gh, repo, pr, queueBase)
				if err == nil {
					d.Action, d.Reason, d.Applied = ActionQueued, "already queued; awaiting actual merge", submitted
					if submitted {
						d.Reason = "queue request accepted; awaiting actual merge"
					}
				}
			case ActionUpdateBranch:
				_, err = gh(ctx, "api", "--method", "PUT", "repos/"+repo+"/pulls/"+strconv.Itoa(pr.Number)+"/update-branch", "-f", "expected_head_sha="+pr.HeadRefOid)
				if err == nil {
					// The update is accepted even if observing it or requesting CI fails.
					d.Applied = true
					var reason string
					reason, err = updatedBranchCI(ctx, gh, repo, pr, authors, deterministic, evidence)
					if reason != "" {
						d.Reason += "; " + reason
					}
				}
			case ActionReview, ActionWait:
				if recoverCI {
					var reason string
					reason, err = dispatchFastChecks(ctx, gh, repo, pr, authors)
					if reason != "" {
						d.Reason += "; " + reason
					}
				}
			}
			if err != nil {
				d.Reason += "; " + err.Error()
				failures = append(failures, fmt.Errorf("settle #%d: %w", pr.Number, err))
			} else if d.Action == ActionMerge || d.Action == ActionUpdateBranch {
				d.Applied = true
			}
		}
		if d.Action == ActionReview && pr.HeadRefName != "" && strings.HasPrefix(pr.HeadRefName, BranchPrefix+"all-") {
			switch gate := fastCheckState(pr); gate {
			case "SUCCESS":
			case "FAILURE", "ERROR", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
				d.Action, d.Reason = ActionBlocked, Gate+" is "+strings.ToLower(gate)+" at the head"
			default:
				d.Action, d.Reason = ActionWait, "waiting for exact-head Fast checks before batch review; "+d.Reason
			}
		}
		decisions = append(decisions, d)
	}
	return decisions, errors.Join(failures...)
}

func jobHeadProof(ctx context.Context, pr pullRequest, deterministic Deterministic, evidence Evidence) (string, error) {
	m := bumpTitle.FindStringSubmatch(pr.Title)
	h, _ := batchHarness(m[1])
	same, err := deterministic(ctx, h, m[2], pr.HeadRefOid)
	if err != nil {
		return "", err
	}
	if !same {
		return "the head is not the job's own bump; a person dispatches its review and merge", nil
	}
	gated, err := evidence(ctx, h, m[2], pr.HeadRefOid)
	if err != nil {
		return "", err
	}
	if !gated {
		return "no bump run on main recorded this head after its gates; a person dispatches its review and merge", nil
	}
	return "", nil
}

func hasFastCheck(pr pullRequest) bool {
	for _, check := range pr.StatusCheckRollup {
		if check.Name == Gate || check.Context == Gate {
			return true
		}
	}
	return false
}

func readPR(ctx context.Context, gh GH, repo string, number int) (pullRequest, error) {
	raw, err := gh(ctx, "pr", "view", strconv.Itoa(number), "--repo", repo, "--json", pullRequestFields)
	if err != nil {
		return pullRequest{}, err
	}
	var pr pullRequest
	if err := json.Unmarshal(raw, &pr); err != nil {
		return pr, fmt.Errorf("read #%d before CI: %w", number, err)
	}
	if pr.Number != number || pr.HeadRefOid == "" || pr.State != "OPEN" {
		return pr, fmt.Errorf("#%d is no longer an open pull request with a known head", number)
	}
	return pr, nil
}

func observeUpdatedPR(ctx context.Context, gh GH, repo string, previous pullRequest) (pullRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for attempt := 0; attempt < 6; attempt++ {
		pr, err := readPR(ctx, gh, repo, previous.Number)
		if err != nil {
			return pr, err
		}
		if pr.HeadRefName != previous.HeadRefName {
			return pr, errors.New("branch changed while observing the update; CI was not requested")
		}
		if pr.HeadRefOid != previous.HeadRefOid {
			return pr, nil
		}
		if attempt < 5 {
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return pr, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return pullRequest{}, errors.New("branch update accepted but its new head is not visible yet; a later settle run will retry missing CI")
}

func updatedBranchCI(ctx context.Context, gh GH, repo string, previous pullRequest, authors map[string]bool, deterministic Deterministic, evidence Evidence) (string, error) {
	updated, err := observeUpdatedPR(ctx, gh, repo, previous)
	if err != nil {
		return "", err
	}
	if err := provenance(updated, authors); err != nil {
		return "", err
	}
	reason, err := jobHeadProof(ctx, updated, deterministic, evidence)
	if err != nil {
		return "", err
	}
	if reason != "" {
		return "", errors.New(reason)
	}
	return dispatchFastChecks(ctx, gh, repo, updated, authors)
}

// A queued dispatch may not have created its aggregate fast-checks job yet.
// A pull_request run awaiting approval does not fulfill this CI handoff.
func dispatchFastChecks(ctx context.Context, gh GH, repo string, pr pullRequest, authors map[string]bool) (string, error) {
	if hasFastCheck(pr) {
		return Gate + " already exists at " + pr.HeadRefOid, nil
	}
	// GitHub holds GITHUB_TOKEN-created PR workflows for approval. A
	// separate dispatch can pass while the PR's merge-ref rollup stays empty.
	// Both callers have already proved the candidate and its build evidence.
	if reason, err := approveFastChecks(ctx, gh, repo, pr, authors); err != nil || reason != "" {
		return reason, err
	}
	raw, err := gh(ctx, "run", "list", "--repo", repo, "--workflow", "ci-fast.yaml", "--branch", pr.HeadRefName,
		"--commit", pr.HeadRefOid, "--event", "workflow_dispatch", "--limit", "1", "--json", "databaseId")
	if err != nil {
		return "", err
	}
	var runs []struct{ DatabaseID int64 }
	if err := json.Unmarshal(raw, &runs); err != nil {
		return "", fmt.Errorf("read Fast checks runs: %w", err)
	}
	if len(runs) != 0 {
		return "Fast checks already has a run at " + pr.HeadRefOid, nil
	}
	current, err := readPR(ctx, gh, repo, pr.Number)
	if err != nil {
		return "", err
	}
	if current.HeadRefOid != pr.HeadRefOid || current.HeadRefName != pr.HeadRefName {
		return "", errors.New("head changed before CI dispatch; settle must prove the new head first")
	}
	if err := provenance(current, authors); err != nil {
		return "", err
	}
	if hasFastCheck(current) {
		return Gate + " already exists at " + pr.HeadRefOid, nil
	}
	if _, err := gh(ctx, "workflow", "run", "ci-fast.yaml", "--repo", repo, "--ref", pr.HeadRefName); err != nil {
		return "", fmt.Errorf("fast checks dispatch failed for %s at %s: %w; settle can retry; the branch must contain ci-fast.yaml with workflow_dispatch (dispatching main would not check this head)", pr.HeadRefName, pr.HeadRefOid, err)
	}
	return "requested Fast checks on " + pr.HeadRefName + " at " + pr.HeadRefOid, nil
}

// approveFastChecks admits execution, never a check result or a PR review.
// It uses existing Actions-write authority and never approves a foreign run.
func approveFastChecks(ctx context.Context, gh GH, repo string, pr pullRequest, authors map[string]bool) (string, error) {
	raw, err := gh(ctx, "run", "list", "--repo", repo, "--workflow", "ci-fast.yaml", "--branch", pr.HeadRefName,
		"--commit", pr.HeadRefOid, "--event", "pull_request", "--status", "action_required", "--limit", "1", "--json", "databaseId,conclusion")
	if err != nil {
		return "", err
	}
	var runs []struct {
		DatabaseID int64
		Conclusion string
	}
	if err := json.Unmarshal(raw, &runs); err != nil {
		return "", fmt.Errorf("read approval-required Fast checks: %w", err)
	}
	if len(runs) == 0 || runs[0].Conclusion != "action_required" {
		return "", nil
	}
	endpoint := "repos/" + repo + "/actions/runs/" + strconv.FormatInt(runs[0].DatabaseID, 10)
	raw, err = gh(ctx, "api", endpoint)
	if err != nil {
		return "", err
	}
	var run struct {
		ID         int64  `json:"id"`
		Event      string `json:"event"`
		Path       string `json:"path"`
		HeadSHA    string `json:"head_sha"`
		HeadBranch string `json:"head_branch"`
		Conclusion string `json:"conclusion"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		HeadRepository struct {
			FullName string `json:"full_name"`
		} `json:"head_repository"`
		PullRequests []struct {
			Number int `json:"number"`
			Head   struct {
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_requests"`
	}
	if err := json.Unmarshal(raw, &run); err != nil {
		return "", fmt.Errorf("read Fast checks approval target: %w", err)
	}
	bound := false
	for _, attached := range run.PullRequests {
		bound = bound || (attached.Number == pr.Number && attached.Head.SHA == pr.HeadRefOid)
	}
	if run.ID <= 0 || run.ID != runs[0].DatabaseID || run.Event != "pull_request" || run.Path != ".github/workflows/ci-fast.yaml" ||
		run.HeadSHA != pr.HeadRefOid || run.HeadBranch != pr.HeadRefName || run.Repository.FullName != repo || run.HeadRepository.FullName != repo || !bound {
		return "", errors.New("approval target is not this proven same-repository Fast checks head")
	}
	if run.Conclusion != "action_required" {
		return "Fast checks approval state changed; a later settle will observe it", nil
	}
	current, err := readPR(ctx, gh, repo, pr.Number)
	if err != nil {
		return "", err
	}
	if current.HeadRefOid != pr.HeadRefOid || current.HeadRefName != pr.HeadRefName {
		return "", errors.New("head changed before workflow approval; settle must prove the new head first")
	}
	if err := provenance(current, authors); err != nil {
		return "", err
	}
	if _, err := gh(ctx, "api", "--method", "POST", endpoint+"/approve"); err != nil {
		return "", fmt.Errorf("approve proven Fast checks run %d: %w", run.ID, err)
	}
	return fmt.Sprintf("approved Fast checks execution for run %d at %s; awaiting its result", run.ID, pr.HeadRefOid), nil
}

// bumpTitle is the title the bump job writes; the name and version it
// carries must match the branch and the catalogue.
var bumpTitle = regexp.MustCompile(`^chore\(provider\): pin ([a-z-]+) (\S+)$`)

// provenance proves a pull request is one the bump job made: same
// repository, base main, a catalogue harness and version named by both the
// branch (`harness-bump/<name>-<version>`) and the title, and no file
// beyond the ones that harness's bump touches. Anything else is ignored: a
// branch name is not a credential.
func provenance(pr pullRequest, authors map[string]bool) error {
	if pr.IsCrossRepository {
		return errors.New("head is in another repository")
	}
	if !authors[strings.TrimSuffix(pr.Author.Login, "[bot]")] && !authors[pr.Author.Login] {
		return fmt.Errorf("opened by %s, not the job", pr.Author.Login)
	}
	if pr.BaseRefName != "main" {
		return errors.New("base is not main")
	}
	m := bumpTitle.FindStringSubmatch(pr.Title)
	if m == nil {
		return errors.New("title is not a bump")
	}
	name, version := m[1], m[2]
	if pr.HeadRefName != BranchPrefix+name+"-"+version {
		return errors.New("branch does not name the harness and version the title names")
	}
	h, err := batchHarness(name)
	if err != nil {
		return errors.New("title names no catalogue harness")
	}
	if !ValidVersion(version) {
		return errors.New("title carries no version")
	}
	allowed := map[string]bool{"Dockerfile": true, "README.md": true}
	if name == "all" {
		if !hexDigest.MatchString(version) {
			return errors.New("batch identity is not a SHA256")
		}
		for _, member := range Catalogue {
			for _, repeat := range member.Repeats {
				allowed[repeat.Path] = true
			}
		}
		allowed["internal/supervisor/provider_profile.go"] = true
		allowed["internal/supervisor/provider_profile_test.go"] = true
	}
	for _, r := range h.Repeats {
		allowed[r.Path] = true
	}
	if h.Name == "claude" {
		allowed["internal/supervisor/provider_profile.go"] = true
		allowed["internal/supervisor/provider_profile_test.go"] = true
	}
	if len(pr.Files) == 0 {
		return errors.New("changes no file")
	}
	for _, f := range pr.Files {
		if !allowed[f.Path] {
			return fmt.Errorf("changes %s, which a %s bump does not touch", f.Path, name)
		}
	}
	return nil
}

func decide(pr pullRequest, botLogin string, authors map[string]bool) Decision {
	d := Decision{Number: pr.Number, Branch: pr.HeadRefName, Head: pr.HeadRefOid, Title: pr.Title}
	if err := provenance(pr, authors); err != nil {
		d.Action, d.Reason = ActionIgnored, "not a bump the job made: "+err.Error()
		return d
	}
	verdict := ""
	for _, r := range pr.Reviews {
		if sameLogin(r.Author.Login, botLogin) && r.Commit.Oid == pr.HeadRefOid {
			switch r.State {
			case "APPROVED", "CHANGES_REQUESTED":
				verdict = r.State
			}
		}
	}
	switch verdict {
	case "":
		d.Action, d.Reason = ActionReview, "no verdict from "+botLogin+" at the head"
		return d
	case "CHANGES_REQUESTED":
		d.Action, d.Reason = ActionChangesRequested, botLogin+" requested changes at the head"
		return d
	}
	gate := fastCheckState(pr)
	switch gate {
	case "SUCCESS":
	case "FAILURE", "ERROR", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
		d.Action, d.Reason = ActionBlocked, Gate+" is "+strings.ToLower(gate)+" at the head"
		return d
	default:
		d.Action, d.Reason = ActionWait, Gate+" has not concluded at the head"
		return d
	}
	switch pr.MergeStateStatus {
	case "BEHIND":
		d.Action, d.Reason = ActionUpdateBranch, "approved and green, but main moved"
	case "CLEAN":
		d.Action, d.Reason = ActionMerge, "approved at the head, "+Gate+" green, clean"
	default:
		d.Action, d.Reason = ActionWait, "approved and green, but the merge state is "+strings.ToLower(pr.MergeStateStatus)
	}
	return d
}

// sameLogin compares GitHub logins with or without the [bot] suffix: the
// API renders an app's reviews as `noise-review` where the UI shows
// `noise-review[bot]`.
func sameLogin(a, b string) bool {
	return strings.TrimSuffix(a, "[bot]") == strings.TrimSuffix(b, "[bot]")
}

const reviewThreadsQuery = `query($owner: String!, $name: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $cursor) {
        nodes { isResolved }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

// unresolvedThreads counts the pull request's unresolved review threads
// across every page.
func unresolvedThreads(ctx context.Context, gh GH, repo string, number int) (int, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return 0, fmt.Errorf("harnessbump: repository %q is not owner/name", repo)
	}
	open, cursor := 0, ""
	for {
		args := []string{"api", "graphql", "-f", "query=" + reviewThreadsQuery, "-F", "owner=" + owner, "-F", "name=" + name, "-F", "number=" + strconv.Itoa(number)}
		if cursor != "" {
			args = append(args, "-F", "cursor="+cursor)
		}
		raw, err := gh(ctx, args...)
		if err != nil {
			return 0, err
		}
		var page struct {
			Data struct {
				Repository struct {
					PullRequest struct {
						ReviewThreads struct {
							Nodes []struct {
								IsResolved bool `json:"isResolved"`
							} `json:"nodes"`
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return 0, fmt.Errorf("harnessbump: review threads of #%d: %w", number, err)
		}
		threads := page.Data.Repository.PullRequest.ReviewThreads
		for _, t := range threads.Nodes {
			if !t.IsResolved {
				open++
			}
		}
		if !threads.PageInfo.HasNextPage {
			return open, nil
		}
		cursor = threads.PageInfo.EndCursor
	}
}

func fastCheckState(pr pullRequest) string {
	gate := ""
	for _, c := range pr.StatusCheckRollup {
		name := c.Name
		if name == "" {
			name = c.Context
		}
		if name != Gate {
			continue
		}
		gate = c.Conclusion
		if gate == "" {
			gate = c.State
		}
		if gate == "" {
			gate = c.Status
		}
	}
	return gate
}
