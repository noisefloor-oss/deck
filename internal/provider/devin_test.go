package provider

import (
	"reflect"
	"testing"
)

const devinSessionID = "sample-session"

func devinDescriptor(t *testing.T) *DevinDescriptor {
	t.Helper()
	d, err := NewDevin("/usr/local/bin/devin", "/home/agent/.config/devin/config.json", HookReceiverPath)
	if err != nil {
		t.Fatalf("NewDevin: %v", err)
	}
	return d
}

func TestDevinDescriptorLaunchResume(t *testing.T) {
	d := devinDescriptor(t)
	if d.Name() != Devin || d.IdentityContract() != IdentityRetrieved {
		t.Fatalf("descriptor identity = %s/%s", d.Name(), d.IdentityContract())
	}
	if _, ok := any(d).(interface{ ExactResumeHookSource() string }); ok {
		t.Fatal("Devin still declares a resume SessionStart grant that 3000.13.4 does not emit")
	}
	if got := d.IdentityHook(); got.Event != "SessionStart" || got.SessionIDField != "session_id" ||
		got.Timing != IdentityHookAtProcessLaunch || got.RequiresNativeTrust {
		t.Fatalf("hook = %+v", got)
	}

	for _, tc := range []struct {
		name       string
		skip       bool
		wantLaunch []string
		wantResume []string
	}{
		{
			name:       "provider permissions",
			wantLaunch: []string{"/usr/local/bin/devin", "--respect-workspace-trust", "false"},
			wantResume: []string{"/usr/local/bin/devin", "--resume", devinSessionID, "--respect-workspace-trust", "false"},
		},
		{
			name:       "user skips permissions",
			skip:       true,
			wantLaunch: []string{"/usr/local/bin/devin", "--respect-workspace-trust", "false", "--permission-mode", "dangerous"},
			wantResume: []string{"/usr/local/bin/devin", "--resume", devinSessionID, "--respect-workspace-trust", "false", "--permission-mode", "dangerous"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := d.NewSessionArgs(LaunchSpec{SkipPermissions: tc.skip})
			if err != nil {
				t.Fatalf("NewSessionArgs: %v", err)
			}
			if !reflect.DeepEqual(got, tc.wantLaunch) {
				t.Fatalf("launch argv = %#v, want %#v", got, tc.wantLaunch)
			}
			got, err = d.ResumeSessionArgs(ResumeSpec{NativeSessionID: devinSessionID, SkipPermissions: tc.skip})
			if err != nil {
				t.Fatalf("ResumeSessionArgs: %v", err)
			}
			if !reflect.DeepEqual(got, tc.wantResume) {
				t.Fatalf("resume argv = %#v, want %#v", got, tc.wantResume)
			}
		})
	}
}

func TestDevinDescriptorRefusesAssignedIDAndReplaysNativeID(t *testing.T) {
	d := devinDescriptor(t)
	if _, err := d.NewSessionArgs(LaunchSpec{SessionID: devinSessionID}); err == nil {
		t.Fatal("assigned id accepted")
	}
	for _, id := range []string{"", "   "} {
		if _, err := d.ResumeSessionArgs(ResumeSpec{NativeSessionID: id}); err == nil {
			t.Fatalf("ResumeSessionArgs accepted empty id %q", id)
		}
	}
	// Any other recorded value is the CLI's to judge: replay it verbatim,
	// with surrounding whitespace dropped.
	for _, tc := range []struct{ recorded, replay string }{
		{"single", "single"},
		{"UPPER-session", "UPPER-session"},
		{"two--parts", "two--parts"},
		{" two-parts", "two-parts"},
		{"two-parts ", "two-parts"},
	} {
		argv, err := d.ResumeSessionArgs(ResumeSpec{NativeSessionID: tc.recorded})
		if err != nil {
			t.Fatalf("ResumeSessionArgs refused recorded id %q: %v", tc.recorded, err)
		}
		if !containsArg(argv, tc.replay) {
			t.Fatalf("resume argv = %v, want the recorded id %q verbatim", argv, tc.replay)
		}
	}
}

func TestDevinDescriptorRuntimeContract(t *testing.T) {
	d := devinDescriptor(t)
	if !d.Injection().Verified || d.Injection().AcceptsInputDuringTurn != ActiveTurnQueued {
		t.Fatalf("injection = %+v", d.Injection())
	}
	if got := d.RequiredConfigPaths(); !reflect.DeepEqual(got, []string{
		"~/.config/devin", "~/.local/share/devin", "/home/agent/.config/devin/config.json",
	}) {
		t.Fatalf("config paths = %#v", got)
	}
	if !d.ExecutableIdentity().Sufficient() {
		t.Fatal("native Devin executable must provide usable /proc identity")
	}
}
