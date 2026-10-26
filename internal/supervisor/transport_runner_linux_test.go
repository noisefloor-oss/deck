package supervisor

import (
	"errors"
	"strings"
	"testing"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// slaveTriple returns three close-on-exec duplicates of the one PTY slave in
func slaveTriple(t *testing.T, slaveFd int) []int {
	t.Helper()
	return []int{
		dupDescriptor(t, slaveFd, true),
		dupDescriptor(t, slaveFd, true),
		dupDescriptor(t, slaveFd, true),
	}
}

func TestAcceptRunnerRequestCanonical(t *testing.T) {
	_, slave := requireMeasurableHost(t)
	rev, err := CanonicalizeLaunchRequest(validLaunchRequest())
	if err != nil {
		t.Fatalf("canonicalize request: %v", err)
	}
	server, client := seqpacketPair(t, true)
	triple := slaveTriple(t, int(slave.Fd()))
	if err := sendSeqpacketMessage(client, []byte(rev.CanonicalJSON), triple); err != nil {
		t.Fatalf("send: %v", err)
	}

	req, ptyBinding, fds, err := AcceptRunnerRequest(server, selfRunner())
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if req.Digest != rev.Digest {
		t.Fatalf("request digest mismatch")
	}
	if ptyBinding == nil {
		t.Fatalf("missing PTY binding")
	}
	if len(fds) != 3 {
		t.Fatalf("fd count = %d, want 3", len(fds))
	}
	// The binding the endpoint derived must equal a direct measurement of the
	// same slave through fresh duplicates.
	direct := slaveTriple(t, int(slave.Fd()))
	measured, err := MeasurePTYBinding(direct[0], direct[1], direct[2])
	if err != nil {
		t.Fatalf("MeasurePTYBinding: %v", err)
	}
	if measured.Digest != ptyBinding.Digest {
		t.Fatalf("endpoint binding differs from direct measurement")
	}
	for _, fd := range fds {
		if !fdIsOpen(fd) {
			t.Fatalf("accepted descriptor unexpectedly closed")
		}
		unix.Close(fd)
	}
}

// sendRejectingRunner sends a runner-shaped message and asserts the endpoint
// rejects it without leaking the descriptors it received.
func sendRejectingRunner(t *testing.T, peer RunnerAdmission, payload []byte, descriptorCount int, want error) {
	t.Helper()
	_, slave := requireMeasurableHost(t)
	server, client := seqpacketPair(t, true)
	var sent []int
	if descriptorCount == runnerPTYDescriptorCount {
		sent = slaveTriple(t, int(slave.Fd()))
	} else if descriptorCount > 0 {
		sent = pipeFDs(t, descriptorCount)
	}
	before := openFDCount(t)
	if err := sendSeqpacketMessage(client, payload, sent); err != nil {
		t.Fatalf("send: %v", err)
	}
	_, _, _, err := AcceptRunnerRequest(server, peer)
	if want != nil && !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if want == nil && err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if after := openFDCount(t); after != before {
		t.Fatalf("receiver leaked descriptors: before=%d after=%d", before, after)
	}
}

func TestAcceptRunnerRequestRejectsWrongPeer(t *testing.T) {
	rev, err := CanonicalizeLaunchRequest(validLaunchRequest())
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	wrongUID := RunnerAdmission{UID: uint32(unix.Getuid()) + 1, PID: int32(unix.Getpid())}
	sendRejectingRunner(t, wrongUID, []byte(rev.CanonicalJSON), runnerPTYDescriptorCount, ErrTransportPeer)
}

func TestAcceptRunnerRequestRejectsSameUIDDifferentPID(t *testing.T) {
	rev, err := CanonicalizeLaunchRequest(validLaunchRequest())
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	wrongPID := RunnerAdmission{UID: uint32(unix.Getuid()), PID: int32(unix.Getpid()) + 9999}
	sendRejectingRunner(t, wrongPID, []byte(rev.CanonicalJSON), runnerPTYDescriptorCount, ErrTransportPeer)
}

func TestAcceptRunnerRequestRejectsNonCanonicalMessage(t *testing.T) {
	rev, err := CanonicalizeLaunchRequest(validLaunchRequest())
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	cases := []struct {
		name    string
		payload []byte
	}{
		{"malformed", []byte("{not json")},
		{"trailing value", []byte(rev.CanonicalJSON + "{}")},
		{"unknown field", []byte(strings.TrimSuffix(rev.CanonicalJSON, "}") + `,"extra":1}`)},
		{"reordered", []byte(reorderLaunchRequestJSON(rev.CanonicalJSON))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sendRejectingRunner(t, selfRunner(), tc.payload, runnerPTYDescriptorCount, ErrTransportMessage)
		})
	}
}

func TestAcceptRunnerRequestRejectsCrossRoleMessage(t *testing.T) {
	// A launcher hello is valid JSON but not a launch request shape; the
	// runner endpoint's own decoder rejects it, so a role cannot speak to the
	// other endpoint's message shape.
	hello, err := CanonicalizeLauncherHello(validLauncherHello())
	if err != nil {
		t.Fatalf("canonicalize hello: %v", err)
	}
	sendRejectingRunner(t, selfRunner(), []byte(hello.CanonicalJSON), runnerPTYDescriptorCount, ErrTransportMessage)
}

func TestAcceptRunnerRequestRejectsNonPTYDescriptors(t *testing.T) {
	rev, err := CanonicalizeLaunchRequest(validLaunchRequest())
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	// Three distinct pipe descriptors pass ancillary shape but fail PTY
	// measurement, so the endpoint closes them and rejects.
	server, client := seqpacketPair(t, true)
	sent := pipeFDs(t, runnerPTYDescriptorCount)
	before := openFDCount(t)
	if err := sendSeqpacketMessage(client, []byte(rev.CanonicalJSON), sent); err != nil {
		t.Fatalf("send: %v", err)
	}
	_, _, _, err = AcceptRunnerRequest(server, selfRunner())
	if err == nil {
		t.Fatalf("expected rejection for non-PTY descriptors")
	}
	if errors.Is(err, ErrPTYBindingUnavailable) {
		t.Fatalf("error must not be the unavailable sentinel: %v", err)
	}
	if after := openFDCount(t); after != before {
		t.Fatalf("receiver leaked descriptors: before=%d after=%d", before, after)
	}
}

func TestAcceptRunnerRequestRejectsPTYMasterRole(t *testing.T) {
	_, slave := requireMeasurableHost(t)
	otherMaster, otherSlave, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	defer otherMaster.Close()
	defer otherSlave.Close()
	rev, err := CanonicalizeLaunchRequest(validLaunchRequest())
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	server, client := seqpacketPair(t, true)
	sent := []int{
		dupDescriptor(t, int(slave.Fd()), true),
		dupDescriptor(t, int(otherMaster.Fd()), true),
		dupDescriptor(t, int(slave.Fd()), true),
	}
	before := openFDCount(t)
	if err := sendSeqpacketMessage(client, []byte(rev.CanonicalJSON), sent); err != nil {
		t.Fatalf("send: %v", err)
	}
	_, _, _, err = AcceptRunnerRequest(server, selfRunner())
	if err == nil {
		t.Fatalf("expected rejection for PTY master")
	}
	if errors.Is(err, ErrPTYBindingUnavailable) {
		t.Fatalf("error must not be the unavailable sentinel: %v", err)
	}
	if after := openFDCount(t); after != before {
		t.Fatalf("receiver leaked descriptors: before=%d after=%d", before, after)
	}
}

func TestAcceptRunnerRequestRejectsDescriptorCount(t *testing.T) {
	rev, err := CanonicalizeLaunchRequest(validLaunchRequest())
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	t.Run("too few", func(t *testing.T) {
		sendRejectingRunner(t, selfRunner(), []byte(rev.CanonicalJSON), 2, ErrTransportAncillary)
	})
	t.Run("too many", func(t *testing.T) {
		sendRejectingRunner(t, selfRunner(), []byte(rev.CanonicalJSON), 4, ErrTransportAncillary)
	})
}

// reorderLaunchRequestJSON produces a valid-JSON, semantically-equal but
// non-canonical body by swapping the first two fields, so the canonical
// re-marshal differs from the received bytes.
func reorderLaunchRequestJSON(canonical string) string {
	return strings.Replace(canonical,
		`{"schema":"noisedeck.supervisor-launch-request.v1","application_session_id":"123e4567-e89b-42d3-a456-426614174000",`,
		`{"application_session_id":"123e4567-e89b-42d3-a456-426614174000","schema":"noisedeck.supervisor-launch-request.v1",`, 1)
}
