package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noisefloor-oss/deck/internal/hookcapture"
	"github.com/noisefloor-oss/deck/internal/provider"
)

const attentionNative = "01988000-1234-7abc-8abc-123456789abc"
const attentionTurn = "12345678-1234-4abc-8abc-123456789abc"

func conversationFixture(t *testing.T) (*Repository, *Session, *hookcapture.Service, string) {
	t.Helper()
	repo, row, cwd := startingCaptureFixture(t)
	ctx := context.Background()
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET provider=? WHERE id=?`, provider.Pi, row.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	service, err := hookcapture.NewService(repo, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.RegisterFenced(ctx, row.ID, string(provider.Pi), cwd, row.Generation, *row.IdentityMarker)
	if err != nil {
		t.Fatal(err)
	}
	if got := postCapture(t, service, token, captureBody(attentionNative, "startup", cwd)); got != 204 {
		t.Fatalf("capture %d", got)
	}
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET status='running' WHERE id=?`, row.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	row, err = repo.Get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	return repo, row, service, token
}

func postConversation(service *hookcapture.Service, token, body string) int {
	req := httptest.NewRequest(http.MethodPost, "/api/hooks/conversation", strings.NewReader(body))
	req.Header.Set("X-Noisedeck-Hook-Token", token)
	res := httptest.NewRecorder()
	service.ConversationHandler()(res, req)
	return res.Code
}

func TestConversationOrderingAndAcknowledgement(t *testing.T) {
	repo, row, service, token := conversationFixture(t)
	ctx := context.Background()
	frames := make(map[int]string)
	lastEnded, lastTurn := int64(0), ""
	emit := func(seq int, state, turn string) {
		t.Helper()
		body, exists := frames[seq]
		if !exists {
			if state == "ended" && turn != lastTurn {
				lastEnded = int64(seq)
				lastTurn = turn
			}
			raw, err := json.Marshal(hookcapture.ConversationEvent{NativeID: attentionNative, PID: strconv.Itoa(*row.PanePID), Sequence: int64(seq), TurnID: turn, State: state, EndedSequence: lastEnded, EndedTurn: lastTurn})
			if err != nil {
				t.Fatal(err)
			}
			body = string(raw)
			frames[seq] = body
		}
		if got := postConversation(service, token, body); got != 204 {
			t.Fatalf("event %d %s: %d", seq, state, got)
		}
	}
	read := func() ConversationAttention {
		t.Helper()
		facts, err := repo.ConversationAttention(ctx, "")
		if err != nil || len(facts) != 1 {
			t.Fatalf("facts=%v err=%v", facts, err)
		}
		return facts[0]
	}
	emit(1, "working", attentionTurn)
	emit(2, "needs_input", attentionTurn)
	if a := read(); a.State != "needs_input" || a.EndedSequence != 0 {
		t.Fatal(a)
	}
	emit(3, "ended", attentionTurn)
	emit(3, "ended", attentionTurn)
	emit(2, "needs_input", attentionTurn)
	emit(4, "ended", attentionTurn)
	first := read()
	if first.State != "ended" || first.EndedSequence != 3 || first.SeenSequence != 0 {
		t.Fatal(first)
	}
	repo = NewRepository(repo.db)
	if a := read(); a.EndedSequence != 3 {
		t.Fatalf("restart lost boundary: %+v", a)
	}
	secondTurn := "22345678-1234-4abc-8abc-123456789abc"
	emit(5, "working", secondTurn)
	emit(6, "ended", secondTurn)
	if err := repo.AcknowledgeConversation(ctx, row.ID, row.Generation, first.ContextKey, first.EndedSequence); err != nil {
		t.Fatal(err)
	}
	if a := read(); a.EndedSequence != 6 || a.SeenSequence != 3 {
		t.Fatalf("old ack hid new boundary: %+v", a)
	}
	if err := repo.AcknowledgeConversation(ctx, row.ID, row.Generation, first.ContextKey, 99); !errors.Is(err, hookcapture.ErrConflict) {
		t.Fatalf("future ack: %v", err)
	}
	emit(7, "needs_input", secondTurn)
	if err := repo.AcknowledgeConversation(ctx, row.ID, row.Generation, first.ContextKey, 6); err != nil {
		t.Fatal(err)
	}
	if a := read(); a.State != "needs_input" || a.SeenSequence != 6 {
		t.Fatalf("view dismissed pending native input: %+v", a)
	}
}

func TestConversationRejectsForeignIdentityAndPayload(t *testing.T) {
	repo, row, service, token := conversationFixture(t)
	ctx := context.Background()
	good := `{"native_id":"` + attentionNative + `","pid":"` + strconv.Itoa(*row.PanePID) + `","sequence":1,"turn_id":"` + attentionTurn + `","state":"ended","ended_sequence":1,"ended_turn":"` + attentionTurn + `"}`
	if got := postConversation(service, "synthetic-invalid-token", good); got != 410 {
		t.Fatalf("unknown credential=%d", got)
	}
	for _, body := range []string{strings.Replace(good, `"state":"ended"`, `"state":"idle"`, 1), strings.Replace(good, `"sequence":1`, `"sequence":-1`, 1), strings.TrimSuffix(good, "}") + `,"prompt":"synthetic forbidden content"}`, good + good} {
		if got := postConversation(service, token, body); got != 400 {
			t.Fatalf("invalid envelope=%d", got)
		}
	}
	wrongPID := strings.Replace(good, `"pid":"`+strconv.Itoa(*row.PanePID)+`"`, `"pid":"1"`, 1)
	if got := postConversation(service, token, wrongPID); got != 409 {
		t.Fatalf("foreign process=%d", got)
	}
	if got := postConversation(service, token, good); got != 204 {
		t.Fatal(got)
	}
	facts, _ := repo.ConversationAttention(ctx, "")
	old := facts[0]
	nextNative := "01988000-1234-7abc-8abc-223456789abc"
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET native_session_id=? WHERE id=?`, nextNative, row.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := postConversation(service, token, good); got != 409 {
		t.Fatalf("old native identity=%d", got)
	}
	if facts, err := repo.ConversationAttention(ctx, ""); err != nil || len(facts) != 0 {
		t.Fatalf("stale context=%v %v", facts, err)
	}
	if err := repo.AcknowledgeConversation(ctx, row.ID, row.Generation, old.ContextKey, old.EndedSequence); !errors.Is(err, hookcapture.ErrConflict) {
		t.Fatalf("old-context ack=%v", err)
	}
	if got := postConversation(service, token, strings.Replace(good, attentionNative, nextNative, 1)); got != 204 {
		t.Fatalf("new native identity=%d", got)
	}
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET generation=generation+1,identity_marker='synthetic-successor' WHERE id=?`, row.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := postConversation(service, token, strings.Replace(good, attentionNative, nextNative, 1)); got != 410 {
		t.Fatalf("old generation=%d", got)
	}
	if facts, err := repo.ConversationAttention(ctx, ""); err != nil || len(facts) != 0 {
		t.Fatalf("old generation fact=%v %v", facts, err)
	}
}

func TestConversationKeepsCompletionAcrossCoalescingAndReload(t *testing.T) {
	for _, carried := range []bool{false, true} {
		name := "late pre-reload completion"
		if carried {
			name = "coalesced completion in a newer snapshot"
		}
		t.Run(name, func(t *testing.T) {
			repo, row, service, token := conversationFixture(t)
			ctx := context.Background()
			send := func(event hookcapture.ConversationEvent) {
				t.Helper()
				raw, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				if got := postConversation(service, token, string(raw)); got != 204 {
					t.Fatalf("delivery=%d", got)
				}
			}
			current := hookcapture.ConversationEvent{NativeID: attentionNative, PID: strconv.Itoa(*row.PanePID), Sequence: 10, TurnID: "22345678-1234-4abc-8abc-123456789abc", State: "working"}
			if carried {
				current.EndedSequence = 3
				current.EndedTurn = attentionTurn
			}
			send(current)
			send(hookcapture.ConversationEvent{NativeID: attentionNative, PID: current.PID, Sequence: 3, TurnID: attentionTurn, State: "ended", EndedSequence: 3, EndedTurn: attentionTurn})
			facts, err := repo.ConversationAttention(ctx, "")
			if err != nil || len(facts) != 1 {
				t.Fatalf("%v %v", facts, err)
			}
			fact := facts[0]
			if fact.State != "working" || fact.Sequence != 10 || fact.EndedSequence != 3 || fact.SeenSequence != 0 {
				t.Fatalf("completion lost or current state rolled back: %+v", fact)
			}
			if err := repo.AcknowledgeConversation(ctx, row.ID, row.Generation, fact.ContextKey, 3); err != nil {
				t.Fatal(err)
			}
			current.Sequence = 11
			current.State = "needs_input"
			current.EndedSequence = 3
			current.EndedTurn = attentionTurn
			send(current)
			facts, err = repo.ConversationAttention(ctx, "")
			if err != nil || facts[0].State != "needs_input" || facts[0].SeenSequence != 3 {
				t.Fatalf("receipt or input need changed: %+v %v", facts, err)
			}
		})
	}
}
