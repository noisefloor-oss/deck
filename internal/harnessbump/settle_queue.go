package harnessbump

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

type bumpQueueSnapshot struct {
	ID          string
	Head        string `json:"headRefOid"`
	Base        string
	State       string
	MergeCommit *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	Entry *struct{ ID string } `json:"mergeQueueEntry"`
}

const bumpQueueQuery = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){ref(qualifiedName:"refs/heads/main"){target{oid}} pullRequest(number:$number){id headRefOid state mergeCommit{oid} mergeQueueEntry{id}}}}`

// ReadReviewSnapshot selects the same live main ref for review and settlement.
func ReadReviewSnapshot(ctx context.Context, gh GH, repo string, number int) (ReviewSnapshot, error) {
	q, err := readBumpQueue(ctx, gh, repo, number)
	if err != nil {
		return ReviewSnapshot{}, err
	}
	s := ReviewSnapshot{Repository: repo, PullRequest: int64(number), Head: q.Head, Base: q.Base}
	return s, s.validate()
}

func readBumpQueue(ctx context.Context, gh GH, repo string, number int) (bumpQueueSnapshot, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return bumpQueueSnapshot{}, errors.New("repository must be owner/name")
	}
	raw, err := gh(ctx, "api", "graphql", "-f", "query="+bumpQueueQuery, "-F", "owner="+owner, "-F", "name="+name, "-F", "number="+strconv.Itoa(number))
	if err != nil {
		return bumpQueueSnapshot{}, err
	}
	var result struct {
		Errors []json.RawMessage
		Data   struct {
			Repository struct {
				PullRequest *bumpQueueSnapshot
				Ref         *struct{ Target struct{ Oid string } }
			}
		}
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return bumpQueueSnapshot{}, err
	}
	q := result.Data.Repository.PullRequest
	if len(result.Errors) > 0 || q == nil || q.ID == "" || q.Head == "" || result.Data.Repository.Ref == nil || result.Data.Repository.Ref.Target.Oid == "" {
		return bumpQueueSnapshot{}, errors.New("queue metadata unavailable")
	}
	if q.State != "OPEN" && q.State != "CLOSED" && q.State != "MERGED" {
		return bumpQueueSnapshot{}, errors.New("pull request state unavailable")
	}
	if q.State == "MERGED" && (q.MergeCommit == nil || q.MergeCommit.OID == "") {
		return bumpQueueSnapshot{}, errors.New("merged pull request has no merge commit")
	}
	// baseRefOid can remain a recorded merge base after main moves.
	// Resolve the branch ref itself instead of trusting the PR field.
	q.Base = result.Data.Repository.Ref.Target.Oid
	return *q, nil
}

func prepareQueueDecision(ctx context.Context, gh GH, repo, bot string, pr pullRequest, d Decision) (Decision, string, error) {
	q, err := readBumpQueue(ctx, gh, repo, pr.Number)
	if err != nil {
		return d, "", err
	}
	if q.Head != pr.HeadRefOid {
		d.Action, d.Reason = ActionWait, "head changed; a later pass must prove it"
		return d, "", nil
	}
	if q.State == "MERGED" {
		d.Action, d.Reason = ActionMerged, "already merged at "+q.MergeCommit.OID
		return d, q.Base, nil
	}
	if q.State == "CLOSED" {
		d.Action, d.Reason = ActionHandOff, "pull request closed without merging; a person must reconcile it"
		return d, q.Base, nil
	}
	if q.Entry != nil {
		d.Action, d.Reason = ActionQueued, "already queued; awaiting actual merge"
		return d, q.Base, nil
	}
	prior, err := bumpQueueIntent(ctx, gh, repo, pr)
	if err != nil {
		return d, "", err
	}
	if prior {
		d.Action, d.Reason = ActionHandOff, "a recorded queue submission is no longer queued; a person must reconcile it"
		return d, q.Base, nil
	}
	raw, err := gh(ctx, "api", "repos/"+repo+"/compare/"+url.PathEscape(q.Base+"..."+q.Head))
	if err != nil {
		return d, "", err
	}
	var comparison struct {
		Behind *int `json:"behind_by"`
	}
	if err := json.Unmarshal(raw, &comparison); err != nil {
		return d, "", err
	}
	if comparison.Behind == nil || *comparison.Behind < 0 {
		return d, "", errors.New("base comparison unavailable")
	}
	if *comparison.Behind > 0 {
		d.Action, d.Reason = ActionUpdateBranch, "main moved; refresh the branch before a new review"
		return d, q.Base, nil
	}
	if d.Action == ActionMerge && !bumpReviewPins(pr, bot, q.Base) {
		d.Action, d.Reason = ActionReview, "approval does not identify the current immutable head and base"
	}
	return d, q.Base, nil
}

func bumpReviewPins(pr pullRequest, bot, base string) bool {
	valid := false
	for _, r := range pr.Reviews {
		if !sameLogin(r.Author.Login, bot) || r.Commit.Oid != pr.HeadRefOid {
			continue
		}
		if r.State != "APPROVED" && r.State != "CHANGES_REQUESTED" {
			continue
		}
		valid = r.State == "APPROVED" && validateReviewBindingBody(r.Body, pr.HeadRefOid, base) == nil
	}
	return valid
}

func bumpQueueMarker(pr pullRequest) string {
	return "<!-- harness-bump-queue-" + strconv.Itoa(pr.Number) + "-" + pr.HeadRefOid + " -->"
}

func bumpQueueIntent(ctx context.Context, gh GH, repo string, pr pullRequest) (bool, error) {
	raw, err := gh(ctx, "api", "repos/"+repo+"/issues/"+strconv.Itoa(pr.Number)+"/comments", "--paginate", "--jq", ".[].body")
	if err != nil {
		return false, err
	}
	return strings.Contains(string(raw), bumpQueueMarker(pr)), nil
}

type bumpQueueResult struct {
	Submitted   bool
	MergeCommit string
}

func enqueueBump(ctx context.Context, gh GH, repo string, pr pullRequest, base string) (bumpQueueResult, error) {
	q, err := readBumpQueue(ctx, gh, repo, pr.Number)
	if err != nil {
		return bumpQueueResult{}, err
	}
	if q.Head != pr.HeadRefOid {
		return bumpQueueResult{}, errors.New("head moved before queue submission")
	}
	if q.State == "MERGED" {
		return bumpQueueResult{MergeCommit: q.MergeCommit.OID}, nil
	}
	if q.State != "OPEN" {
		return bumpQueueResult{}, errors.New("pull request closed before queue submission")
	}
	if q.Base != base {
		return bumpQueueResult{}, errors.New("head or base moved before queue submission")
	}
	if q.Entry != nil {
		return bumpQueueResult{}, nil
	}
	prior, err := bumpQueueIntent(ctx, gh, repo, pr)
	if err != nil {
		return bumpQueueResult{}, err
	}
	if prior {
		return bumpQueueResult{}, errors.New("queue intent already recorded; observe rather than resubmit")
	}
	// This marker can only stop a submission, never authorize one. Record
	// before the write so a lost response cannot cause repeated queue builds.
	if _, err := gh(ctx, "api", "--method", "POST", "repos/"+repo+"/issues/"+strconv.Itoa(pr.Number)+"/comments", "-f", "body=Harness updater: submitting reviewed head to the merge queue; admission is not completion.\n\n"+bumpQueueMarker(pr)); err != nil {
		return bumpQueueResult{}, err
	}
	const query = `mutation($id:ID!,$head:GitObjectID!){enqueuePullRequest(input:{pullRequestId:$id,expectedHeadOid:$head}){mergeQueueEntry{id}}}`
	raw, err := gh(ctx, "api", "graphql", "-f", "query="+query, "-f", "id="+q.ID, "-f", "head="+pr.HeadRefOid)
	if err != nil {
		return bumpQueueResult{}, err
	}
	var receipt struct {
		Errors []json.RawMessage
		Data   struct {
			Enqueue struct {
				Entry *struct{ ID string } `json:"mergeQueueEntry"`
			} `json:"enqueuePullRequest"`
		}
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return bumpQueueResult{}, err
	}
	if len(receipt.Errors) > 0 || receipt.Data.Enqueue.Entry == nil || receipt.Data.Enqueue.Entry.ID == "" {
		return bumpQueueResult{}, errors.New("queue admission was refused or unverifiable; manual reconciliation required")
	}
	return bumpQueueResult{Submitted: true}, nil
}
