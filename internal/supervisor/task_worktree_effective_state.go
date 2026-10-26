package supervisor

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var ErrTaskWorktreeEffectiveState = errors.New("task worktree workload effective state is unsafe")

const (
	TaskWorktreeManifestSchemaV2          = "noisedeck.task-worktree-profile.v2"
	TaskWorktreeBaselineSchemaV2          = "noisedeck.task-worktree-mount-baseline.v2"
	TaskWorktreeReceiptSchemaV2           = "noisedeck.task-worktree-source-receipt.v2"
	TaskWorktreeContainerProfile          = "docker-task-workload-v1"
	TaskWorktreeHostQualificationSchemaV2 = "noisedeck.task-worktree-host-qualification.v2"
	TaskWorktreeRuntime                   = "runc"
	TaskWorktreeLatchPath                 = "/usr/local/libexec/noisedeck-task-workload-latch"
	TaskWorktreeSourceMagic               = "magic-proc-fd"
	TaskWorktreeSourceCanonical           = "canonical-host-path"
	TaskWorktreeUID                       = uint32(1026)
	TaskWorktreeGID                       = uint32(100)
	maxTaskDockerInspectBytes             = 4 << 20
	maxTaskMountInfoBytes                 = 4 << 20
	maxTaskProcessStatusBytes             = 1 << 20
)

const (
	taskWorktreeLabelSchema      = "com.noisedeck.phase-a.schema"
	taskWorktreeLabelRunID       = "com.noisedeck.phase-a.run-id"
	taskWorktreeLabelProfile     = "com.noisedeck.phase-a.profile-revision"
	taskWorktreeLabelBuild       = "com.noisedeck.phase-a.qualifier-build"
	taskWorktreeLabelRequestCore = "com.noisedeck.phase-a.request-core"
	taskWorktreePhaseASchemaV1   = "noisedeck.task-worktree-phase-a.v1"
)

type TaskWorktreeMountExpectation struct {
	Role, Type, Source, ReceiptID, Destination, Propagation string
	ReadOnly                                                bool
}

type TaskWorktreeMountRole struct {
	Role, Destination, RelativeRole, SourceKind string
	ReadOnly                                    bool
}

// TaskWorktreeSourceReceipt is acquired from a retained descriptor by the
// trusted lifecycle owner. Mutable directories are identities, never content hashes.
type TaskWorktreeSourceReceipt struct {
	Schema, ReceiptID, ProtectedBackingID, TasksReceiptID string
	Role, RelativeRole, Kind                              string
	MountID, Device, Inode, HeldFDGeneration              uint64
	OwnerUID, OwnerGID                                    uint32
	QualifierPID                                          int
	HeldFD                                                uint32
}

type TaskWorktreeRuntimeMount struct {
	Class, Root, Destination, MajorMinor string
	SourceClass, ReceiptID               string
	ReceiptMountID, ReceiptDevice        uint64
	MountOptions, OptionalFields         []string
	FSType, Source                       string
	SuperOptions                         []string
}

type TaskWorktreeRuntimeBaseline struct {
	Schema          string
	Digest          string
	Mounts          []TaskWorktreeRuntimeMount
	QualifiedAbsent []string
}

// TaskWorktreeProtectedManifest is the source-checkpoint model loaded by a
// protected identity. It is not the Phase A wire schema.
type TaskWorktreeProtectedManifest struct {
	Schema, Profile, AuthorityRevision, Digest         string
	ProviderProfileID, ProviderProfileRevision         string
	RunID, Hostname, QualifierBuildDigest              string
	RequestCoreDigest, HostQualificationDigest         string
	ImageID, ContainerID, TiniSHA256, LatchSHA256      string
	User, Runtime                                      string
	Entrypoint, ProviderArgv, ConfigEnv                []string
	WorkingDir, NetworkMode, NetworkID, DockerDataRoot string
	ProviderExecutable                                 TaskWorktreeExecutableReceipt
	Console                                            TaskWorktreeConsoleReceipt
	SecurityOpt                                        []string
	Tmpfs                                              map[string]string
	MaskedPaths, ReadonlyPaths                         []string
	Mounts                                             []TaskWorktreeMountExpectation
	Receipts                                           []TaskWorktreeSourceReceipt
	RuntimeBaseline                                    TaskWorktreeRuntimeBaseline
}

type TaskWorktreeExecutableReceipt struct {
	Path, SHA256, Kind     string
	MountID, Device, Inode uint64
}

type TaskWorktreeNamespaceState struct{ Host, Container map[string]string }

type TaskWorktreeObservedSource struct {
	ReceiptID, DockerSource, Kind            string
	MountID, Device, Inode, HeldFDGeneration uint64
	OwnerUID, OwnerGID                       uint32
	Readable, Writable                       bool
}

type TaskWorktreeIDMap struct{ Inside, Outside, Length uint64 }

// TaskWorktreeHostQualification is a protected source-checkpoint model, not
// the signed Phase A host-entry wire schema.
type TaskWorktreeHostQualification struct {
	Schema, QualificationID, DockerEngineID, Runtime  string
	DockerInfoSHA256, UsernsRemapIdentity, ObserverID string
	ProviderProfileID, ProviderProfileRevision        string
	RequestCoreDigest, RuntimeBaselineDigest          string
	CollectorSHA256, Digest                           string
	MountSourceMode                                   string
	ReportedSources                                   []TaskWorktreeReportedSource
	QualifiedAbsent                                   []TaskWorktreeAbsentPathQualification
}

type TaskWorktreeReportedSource struct {
	ReceiptID, Source string
}

type TaskWorktreeAbsentPathQualification struct {
	Path, ObserverID, CandidateSetDigest, HostMountNamespace string
	HostRootReceiptID, LstatState, Errno                     string
}

type TaskWorktreeAbsentPathObservation struct {
	Path, QualificationID, ObserverID, ContainerID string
	ProcRootReceiptID, LstatState, Errno           string
	InitPID                                        int
}

type TaskWorktreeEnvironmentObservation struct {
	Role, QualificationID, ObserverID, ContainerID string
	ExecID, ExecutableSHA256                       string
	InitPID, ProcessPID                            int
	ProcessStartTime                               uint64
	Complete                                       bool
	Entries                                        []string
}

type TaskWorktreeEndpointObservation struct {
	ObserverID, ContainerID, Path, LstatState, FileType, ConnectResult string
	InitPID                                                            int
}

type TaskWorktreeInheritedFD struct {
	Number                              int
	ReceiptID, Kind, Target, MajorMinor string
	Device, Inode                       uint64
}

type TaskWorktreeConsoleReceipt struct {
	ReceiptID, Kind, Target, MajorMinor string
	Device, Inode                       uint64
}

type TaskWorktreeRuntimeIdentity struct {
	ContainerID                string
	InitPID, DockerInspectPID  int
	InitDevice, InitInode      uint64
	InitSHA256                 string
	EffectiveUID, EffectiveGID uint32
	UIDMap, GIDMap             []TaskWorktreeIDMap
	Sources                    []TaskWorktreeObservedSource
	Endpoints                  []TaskWorktreeEndpointObservation
	InheritedFDs               []TaskWorktreeInheritedFD
	AbstractSockets            []string
	Processes                  []TaskWorktreeProcessIdentity
	ProviderExecutable         TaskWorktreeExecutableReceipt
	OpenUnixSockets            TaskWorktreeUnixSocketEvidence
	Environments               []TaskWorktreeEnvironmentObservation
	QualifiedAbsent            []TaskWorktreeAbsentPathObservation
	ProcRootReceiptID          string
	CollectorExecID            string
	CollectorExecPID           int
	CollectorExecStartTime     uint64
	CollectorSHA256            string
}

type TaskWorktreeUnixSocketEvidence struct {
	ObserverID, ContainerID string
	InitPID                 int
	Complete                bool
	Filesystem, Abstract    []string
}

type TaskWorktreeProcessIdentity struct {
	PID, PPID                     int
	ExecutableSHA256, Role, State string
	Cmdline                       []string
}

type TaskWorktreeGitBindingState struct{ DotGit, Gitdir, Commondir, WorktreeConfig string }

type dockerTaskInspect struct {
	ID, Image, Path string
	Args            []string
	RestartCount    int
	State           struct {
		Status, Error, StartedAt, FinishedAt         string
		Running, Paused, Restarting, OOMKilled, Dead bool
		Pid, ExitCode                                int
	}
	Config struct {
		User, Hostname, WorkingDir, StopSignal  string
		StopTimeout                             json.RawMessage
		Entrypoint, Cmd, Env                    []string
		Labels                                  map[string]string
		AttachStdin, AttachStdout, AttachStderr bool
		OpenStdin, StdinOnce, Tty               bool
		ExposedPorts                            map[string]json.RawMessage
		Healthcheck                             *struct{ Test []string }
	}
	HostConfig struct {
		ReadonlyRootfs, Privileged, AutoRemove, Init, PublishAllPorts    bool
		CapAdd, CapDrop, SecurityOpt, GroupAdd, VolumesFrom, ExtraHosts  []string
		NetworkMode, PidMode, IpcMode, UsernsMode, CgroupnsMode, Runtime string
		CgroupParent, UTSMode, Isolation, ContainerIDFile                string
		DeviceCgroupRules, Links                                         []string
		RestartPolicy                                                    struct {
			Name              string
			MaximumRetryCount int
		}
		LogConfig struct {
			Type   string
			Config map[string]string
		}
		Devices, DeviceRequests    []json.RawMessage
		Sysctls, Tmpfs             map[string]string
		MaskedPaths, ReadonlyPaths []string
		PortBindings               map[string]json.RawMessage
	}
	Mounts []struct {
		Type, Source, Destination, Mode, Propagation string
		RW                                           bool
	}
	NetworkSettings struct {
		Networks map[string]struct{ NetworkID string }
	}
}

var taskWorktreeMountVector = []TaskWorktreeMountRole{
	{"assigned_worktree", "/project", "assigned-worktree", "directory", false},
	{"assigned_dot_git", "/project/.git", "assigned-worktree-dot-git", "regular", true},
	{"git_common", "/project.git", "git-common", "directory", false},
	{"git_hooks", "/project.git/hooks", "git-common-hooks", "directory", true},
	{"git_config", "/project.git/config", "git-common-config", "regular", true},
	{"git_config_worktree", "/project.git/config.worktree", "git-common-config-worktree", "regular", true},
	{"git_info", "/project.git/info", "git-common-info", "directory", true},
	{"git_objects", "/project.git/objects", "git-objects", "directory", false},
	{"git_objects_info", "/project.git/objects/info", "git-objects-info", "directory", false},
	{"git_objects_alternates", "/project.git/objects/info/alternates", "git-objects-alternates", "regular", true},
	{"worktree_registry", "/project.git/worktrees", "worktree-registry", "directory", true},
	{"assigned_git_admin", "/project.git/worktrees/assigned", "assigned-git-admin", "directory", false},
	{"assigned_gitdir", "/project.git/worktrees/assigned/gitdir", "assigned-gitdir", "regular", true},
	{"assigned_commondir", "/project.git/worktrees/assigned/commondir", "assigned-commondir", "regular", true},
	{"assigned_config", "/project.git/worktrees/assigned/config.worktree", "assigned-config-worktree", "regular", true},
}

var taskWorktreeConfigEnv = []string{
	"HOME=/home/agent", "PATH=/usr/local/bin:/usr/bin:/bin", "NODE_VERSION=26.12.1", "YARN_VERSION=1.22.22", "TERM=xterm-256color",
}

var taskWorktreeMaskedPaths = []string{
	"/proc/acpi", "/proc/asound", "/proc/interrupts", "/proc/kcore", "/proc/keys", "/proc/latency_stats", "/proc/sched_debug", "/proc/scsi", "/proc/timer_list", "/proc/timer_stats", "/sys/devices/virtual/powercap", "/sys/firmware",
}

var taskWorktreeReadonlyPaths = []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"}

var taskWorktreeTmpfs = map[string]string{
	"/home/agent":    "rw,noexec,nosuid,nodev,size=512m,uid=1026,gid=100,mode=0700",
	"/run/noisedeck": "rw,noexec,nosuid,nodev,size=16m,uid=1026,gid=100,mode=0700",
	"/tmp":           "rw,exec,nosuid,nodev,size=2g,uid=1026,gid=100,mode=1777",
}

var taskWorktreeEndpointPaths = []string{
	"/run/containerd/containerd.sock", "/run/dbus/system_bus_socket", "/run/docker.sock",
	"/run/noisedeck/supervisor.sock", "/run/podman/podman.sock", "/run/systemd/private", "/var/run/docker.sock",
}

var enginePseudoExact = map[string]struct{}{
	"/": {}, "/dev": {}, "/dev/mqueue": {}, "/dev/pts": {}, "/dev/shm": {},
	"/etc/hostname": {}, "/etc/hosts": {}, "/etc/resolv.conf": {}, "/proc": {},
	"/proc/acpi": {}, "/proc/bus": {}, "/proc/fs": {}, "/proc/interrupts": {},
	"/proc/asound": {}, "/proc/irq": {}, "/proc/kcore": {}, "/proc/keys": {}, "/proc/latency_stats": {},
	"/proc/sched_debug": {}, "/proc/scsi": {}, "/proc/sys": {}, "/proc/sysrq-trigger": {}, "/proc/timer_list": {}, "/proc/timer_stats": {},
	"/sys": {}, "/sys/firmware": {}, "/sys/fs/cgroup": {}, "/sys/devices/virtual/powercap": {},
}
var thermalMount = regexp.MustCompile(`^/sys/devices/system/cpu/cpu[0-9]+/thermal_throttle$`)

func CanonicalTaskWorktreeTmpfs() map[string]string { return cloneMap(taskWorktreeTmpfs) }
func CanonicalTaskWorktreeConfigEnv() []string      { return slices.Clone(taskWorktreeConfigEnv) }
func CanonicalTaskWorktreeMaskedPaths() []string    { return slices.Clone(taskWorktreeMaskedPaths) }
func CanonicalTaskWorktreeReadonlyPaths() []string  { return slices.Clone(taskWorktreeReadonlyPaths) }
func CanonicalTaskWorktreeEndpointPaths() []string  { return slices.Clone(taskWorktreeEndpointPaths) }
func CanonicalTaskWorktreeCoverCandidates() []string {
	return append(CanonicalTaskWorktreeMaskedPaths(), taskWorktreeReadonlyPaths...)
}
func CanonicalTaskWorktreeEnginePseudoDestinations() []string {
	result := make([]string, 0, len(enginePseudoExact))
	for destination := range enginePseudoExact {
		result = append(result, destination)
	}
	slices.Sort(result)
	return result
}

// ValidateTaskWorktreeRuntimeBaseline applies the source-checkpoint mount
// policy to independently decoded evidence. Callers remain responsible for
// authenticating the baseline and receipt objects before invoking it.
func ValidateTaskWorktreeRuntimeBaseline(baseline TaskWorktreeRuntimeBaseline, receipts []TaskWorktreeSourceReceipt, containerID, dockerDataRoot string) bool {
	return validateRuntimeBaseline(baseline, receipts, containerID, dockerDataRoot)
}

func CanonicalTaskWorktreeMountRoles() []TaskWorktreeMountRole {
	return slices.Clone(taskWorktreeMountVector)
}

func canonicalTaskWorktreeLabels(manifest TaskWorktreeProtectedManifest) map[string]string {
	return map[string]string{
		taskWorktreeLabelSchema:      taskWorktreePhaseASchemaV1,
		taskWorktreeLabelRunID:       manifest.RunID,
		taskWorktreeLabelProfile:     manifest.ProviderProfileRevision,
		taskWorktreeLabelBuild:       manifest.QualifierBuildDigest,
		taskWorktreeLabelRequestCore: manifest.RequestCoreDigest,
	}
}

func CanonicalTaskWorktreeManifestDigest(manifest TaskWorktreeProtectedManifest) (string, error) {
	manifest.Digest, manifest.RuntimeBaseline.Digest = "", ""
	raw, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func CanonicalTaskWorktreeBaselineDigest(baseline TaskWorktreeRuntimeBaseline) (string, error) {
	baseline.Digest = ""
	raw, err := json.Marshal(baseline)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func canonicalTaskWorktreeQualificationDigest(qualification TaskWorktreeHostQualification) (string, error) {
	qualification.Digest = ""
	raw, err := json.Marshal(qualification)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func VerifyTaskWorktreeDockerInspect(raw []byte, manifest TaskWorktreeProtectedManifest, qualification TaskWorktreeHostQualification) error {
	if err := validateManifest(manifest); err != nil {
		return err
	}
	if err := validateQualification(manifest, qualification); err != nil {
		return err
	}
	if len(raw) == 0 || len(raw) > maxTaskDockerInspectBytes {
		return unsafeTaskState("Docker inspect size is invalid")
	}
	var list []dockerTaskInspect
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&list); err != nil || len(list) != 1 {
		return unsafeTaskState("malformed Docker inspect")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return unsafeTaskState("trailing Docker inspect data")
	}
	i := list[0]
	wantArgs := append([]string{"-g", "--", TaskWorktreeLatchPath}, manifest.ProviderArgv...)
	if i.ID != manifest.ContainerID || i.Image != manifest.ImageID || i.Path != "/usr/bin/tini" || !slices.Equal(i.Args, wantArgs) || i.RestartCount != 0 || i.Config.User != manifest.User ||
		i.Config.Hostname != manifest.Hostname || !equalStringMap(i.Config.Labels, canonicalTaskWorktreeLabels(manifest)) ||
		!slices.Equal(i.Config.Entrypoint, manifest.Entrypoint) || !slices.Equal(i.Config.Cmd, manifest.ProviderArgv) ||
		!slices.Equal(i.Config.Env, manifest.ConfigEnv) || i.Config.WorkingDir != manifest.WorkingDir || i.Config.StopSignal != "SIGTERM" || !bytes.Equal(i.Config.StopTimeout, []byte("null")) ||
		!i.Config.AttachStdin || !i.Config.AttachStdout || !i.Config.AttachStderr || !i.Config.OpenStdin || i.Config.StdinOnce || !i.Config.Tty ||
		len(i.Config.ExposedPorts) != 0 || i.Config.Healthcheck == nil || !slices.Equal(i.Config.Healthcheck.Test, []string{"NONE"}) {
		return unsafeTaskState("image process profile differs")
	}
	if !i.State.Running || i.State.Paused || i.State.Restarting || i.State.OOMKilled || i.State.Dead || i.State.Status != "running" ||
		i.State.Pid <= 0 || i.State.ExitCode != 0 || i.State.Error != "" || i.State.StartedAt == "" || i.State.FinishedAt != "0001-01-01T00:00:00Z" {
		return unsafeTaskState("latch is not the inert running process")
	}
	h := i.HostConfig
	if !h.ReadonlyRootfs || h.Privileged || h.AutoRemove || h.Init || h.PublishAllPorts || len(h.CapAdd) != 0 ||
		!equalFoldExact(h.CapDrop, []string{"ALL"}) || !slices.Equal(h.SecurityOpt, manifest.SecurityOpt) || h.Runtime != manifest.Runtime ||
		h.NetworkMode != manifest.NetworkMode || h.PidMode != "" || h.IpcMode != "private" || h.UsernsMode != "" || h.CgroupnsMode != "private" ||
		h.CgroupParent != "" || h.UTSMode != "" || h.Isolation != "" || h.ContainerIDFile != "" || len(h.DeviceCgroupRules) != 0 || len(h.Links) != 0 || h.RestartPolicy.Name != "no" ||
		h.RestartPolicy.MaximumRetryCount != 0 || h.LogConfig.Type != "none" || len(h.LogConfig.Config) != 0 || len(h.GroupAdd) != 0 ||
		len(h.VolumesFrom) != 0 || len(h.ExtraHosts) != 0 || len(h.Devices) != 0 || len(h.DeviceRequests) != 0 || len(h.Sysctls) != 0 ||
		len(h.PortBindings) != 0 || !equalStringMap(h.Tmpfs, manifest.Tmpfs) || !slices.Equal(h.MaskedPaths, manifest.MaskedPaths) ||
		!slices.Equal(h.ReadonlyPaths, manifest.ReadonlyPaths) {
		return unsafeTaskState("Docker host profile differs")
	}
	if len(i.NetworkSettings.Networks) != 1 || i.NetworkSettings.Networks[manifest.NetworkMode].NetworkID != manifest.NetworkID {
		return unsafeTaskState("protected Docker network differs")
	}
	if len(i.Mounts) != len(manifest.Mounts) {
		return unsafeTaskState("Docker mount count differs")
	}
	observedMounts := make(map[string]struct {
		Type, Source, Destination, Mode, Propagation string
		RW                                           bool
	}, len(i.Mounts))
	for _, mount := range i.Mounts {
		if _, duplicate := observedMounts[mount.Destination]; duplicate {
			return unsafeTaskState("duplicate Docker mount destination")
		}
		observedMounts[mount.Destination] = mount
	}
	for n, want := range manifest.Mounts {
		got, ok := observedMounts[want.Destination]
		expectedSource := expectedInspectSource(qualification, n)
		if !ok || got.Type != want.Type || got.Source != expectedSource || got.Destination != want.Destination || got.RW == want.ReadOnly ||
			got.Propagation != want.Propagation || !mountModeMatches(got.Mode, want.ReadOnly) {
			return unsafeTaskState("Docker mount vector differs at %s", want.Role)
		}
	}
	return nil
}

func VerifyTaskWorktreeRuntimeState(mountInfo, processStatus []byte, manifest TaskWorktreeProtectedManifest, qualification TaskWorktreeHostQualification, identity TaskWorktreeRuntimeIdentity) error {
	if err := validateManifest(manifest); err != nil {
		return err
	}
	if err := validateQualification(manifest, qualification); err != nil {
		return err
	}
	if len(mountInfo) == 0 || len(mountInfo) > maxTaskMountInfoBytes || len(processStatus) == 0 || len(processStatus) > maxTaskProcessStatusBytes {
		return unsafeTaskState("runtime evidence size is invalid")
	}
	actual, err := parseTaskMountInfo(mountInfo)
	if err != nil || !runtimeMountsEqual(actual, manifest.RuntimeBaseline.Mounts) {
		return unsafeTaskState("runtime mount table differs")
	}
	if err := verifyRuntimeIdentity(manifest, identity, qualification); err != nil {
		return err
	}
	status := parseStatus(processStatus)
	if !strings.HasPrefix(status["State"], "T ") {
		return unsafeTaskState("latch process is not stopped")
	}
	for _, key := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
		if value := status[key]; value == "" || strings.Trim(value, "0") != "" {
			return unsafeTaskState("%s is not empty", key)
		}
	}
	if status["NoNewPrivs"] != "1" || status["Uid"] != "1026\t1026\t1026\t1026" || status["Gid"] != "100\t100\t100\t100" {
		return unsafeTaskState("runtime identity or no-new-privileges differs")
	}
	return nil
}

func VerifyTaskWorktreeNamespaceState(state TaskWorktreeNamespaceState) error {
	for _, name := range []string{"mnt", "pid", "ipc", "net", "user", "cgroup"} {
		host, hostOK := state.Host[name]
		container, containerOK := state.Container[name]
		if !hostOK || !containerOK || host == "" || container == "" || host == container {
			return unsafeTaskState("%s namespace is not independently observed", name)
		}
	}
	return nil
}

func VerifyTaskWorktreeGitBinding(state TaskWorktreeGitBindingState) error {
	if state.DotGit != "gitdir: /project.git/worktrees/assigned\n" || state.Gitdir != "/project/.git\n" || state.Commondir != "../..\n" ||
		state.WorktreeConfig != "absent" && state.WorktreeConfig != "false" {
		return unsafeTaskState("protected Git binding differs")
	}
	return nil
}

func validateManifest(m TaskWorktreeProtectedManifest) error {
	baselineDigest, err := CanonicalTaskWorktreeBaselineDigest(m.RuntimeBaseline)
	if err != nil || !isLowerSHA256(m.RuntimeBaseline.Digest) || baselineDigest != m.RuntimeBaseline.Digest || !validateRuntimeBaseline(m.RuntimeBaseline, m.Receipts, m.ContainerID, m.DockerDataRoot) {
		return unsafeTaskState("invalid protected runtime baseline")
	}
	digest, err := CanonicalTaskWorktreeManifestDigest(m)
	if err != nil || !isLowerSHA256(m.Digest) || digest != m.Digest || m.Schema != TaskWorktreeManifestSchemaV2 || m.Profile != TaskWorktreeContainerProfile ||
		m.AuthorityRevision == "" || m.ProviderProfileID == "" || m.ProviderProfileRevision == "" || !validRunID(m.RunID) || m.Hostname != "ndq-"+m.RunID || !isLowerSHA256(m.QualifierBuildDigest) || !isLowerSHA256(m.RequestCoreDigest) || !isLowerSHA256(m.HostQualificationDigest) ||
		!validImageID(m.ImageID) || !isLowerSHA256(m.ContainerID) || !isLowerSHA256(m.TiniSHA256) || !isLowerSHA256(m.LatchSHA256) ||
		m.User != "1026:100" || m.Runtime != TaskWorktreeRuntime || !slices.Equal(m.Entrypoint, []string{"/usr/bin/tini", "-g", "--", TaskWorktreeLatchPath}) ||
		len(m.ProviderArgv) == 0 || !filepath.IsAbs(m.ProviderArgv[0]) || filepath.Clean(m.ProviderArgv[0]) != m.ProviderArgv[0] || m.ProviderArgv[0] == TaskWorktreeLatchPath || m.ProviderArgv[0] != m.ProviderExecutable.Path ||
		!validProviderExecutable(m.ProviderExecutable) || !validConsoleReceipt(m.Console) || m.WorkingDir != "/project" || !validTaskNetwork(m.NetworkMode) || !isLowerSHA256(m.NetworkID) || !validDockerDataRoot(m.DockerDataRoot) || !slices.Equal(m.ConfigEnv, taskWorktreeConfigEnv) ||
		!slices.Equal(m.SecurityOpt, []string{"no-new-privileges:true"}) || !equalStringMap(m.Tmpfs, taskWorktreeTmpfs) ||
		!slices.Equal(m.MaskedPaths, taskWorktreeMaskedPaths) || !slices.Equal(m.ReadonlyPaths, taskWorktreeReadonlyPaths) ||
		len(m.Mounts) != len(taskWorktreeMountVector) || len(m.Receipts) != len(taskWorktreeMountVector) {
		return unsafeTaskState("invalid protected profile manifest")
	}
	receipts, objects, sources := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	var backing, tasks string
	qualifierPID := 0
	for n, fixed := range taskWorktreeMountVector {
		mount, receipt := m.Mounts[n], m.Receipts[n]
		if mount.Role != fixed.Role || mount.Type != "bind" || mount.Destination != fixed.Destination || mount.ReadOnly != fixed.ReadOnly ||
			mount.Propagation != "rprivate" || receipt.QualifierPID <= 0 || receipt.HeldFD < 3 || mount.Source != fmt.Sprintf("/proc/%d/fd/%d", receipt.QualifierPID, receipt.HeldFD) || mount.ReceiptID == "" ||
			receipt.Schema != TaskWorktreeReceiptSchemaV2 || receipt.ReceiptID != mount.ReceiptID || receipt.Role != fixed.Role ||
			receipt.RelativeRole != fixed.RelativeRole || receipt.Kind != fixed.SourceKind || receipt.MountID == 0 || receipt.Device == 0 || receipt.Inode == 0 ||
			receipt.HeldFDGeneration == 0 || receipt.ProtectedBackingID == "" || receipt.TasksReceiptID == "" {
			return unsafeTaskState("invalid protected mount receipt at %s", fixed.Role)
		}
		if n == 0 {
			backing, tasks = receipt.ProtectedBackingID, receipt.TasksReceiptID
			qualifierPID = receipt.QualifierPID
		}
		if receipt.ProtectedBackingID != backing || receipt.TasksReceiptID != tasks || receipt.QualifierPID != qualifierPID {
			return unsafeTaskState("receipt authority differs")
		}
		if _, duplicate := receipts[receipt.ReceiptID]; duplicate {
			return unsafeTaskState("duplicate receipt identity")
		}
		receipts[receipt.ReceiptID] = struct{}{}
		if _, duplicate := sources[mount.Source]; duplicate {
			return unsafeTaskState("mount source path is reused")
		}
		sources[mount.Source] = struct{}{}
		object := fmt.Sprintf("%d:%d:%d", receipt.MountID, receipt.Device, receipt.Inode)
		if _, duplicate := objects[object]; duplicate {
			return unsafeTaskState("receipt sources alias")
		}
		objects[object] = struct{}{}
	}
	return nil
}

func validateRuntimeBaseline(b TaskWorktreeRuntimeBaseline, receipts []TaskWorktreeSourceReceipt, containerID, dockerDataRoot string) bool {
	if b.Schema != TaskWorktreeBaselineSchemaV2 || len(b.Mounts) == 0 || !validQualifiedAbsentPaths(b.QualifiedAbsent) {
		return false
	}
	seen := map[string]struct{}{}
	absent := make(map[string]struct{}, len(b.QualifiedAbsent))
	for _, path := range b.QualifiedAbsent {
		absent[path] = struct{}{}
	}
	receiptByRole := map[string]TaskWorktreeSourceReceipt{}
	for _, receipt := range receipts {
		receiptByRole[receipt.Role] = receipt
	}
	for _, mount := range b.Mounts {
		if mount.Destination == "" {
			return false
		}
		if _, duplicate := seen[mount.Destination]; duplicate {
			return false
		}
		if _, missing := absent[mount.Destination]; missing {
			return false
		}
		seen[mount.Destination] = struct{}{}
		switch mount.Class {
		case "topology":
			fixed, ok := fixedMountAt(mount.Destination)
			if !ok || slices.Contains(mount.MountOptions, "ro") != fixed.ReadOnly || slices.Contains(mount.MountOptions, "rw") == fixed.ReadOnly {
				return false
			}
			receipt := receiptByRole[fixed.Role]
			mountDevice, deviceOK := majorMinorDevice(mount.MajorMinor)
			if mount.SourceClass != "project-backing" || mount.ReceiptID != receipt.ReceiptID || mount.ReceiptMountID != receipt.MountID || mount.ReceiptDevice != receipt.Device ||
				!deviceOK || mountDevice != receipt.Device || !filepath.IsAbs(mount.Root) || filepath.Clean(mount.Root) != mount.Root || mount.FSType == "" || mount.Source == "" ||
				!hasOptions(mount.MountOptions, "nosuid", "nodev") || !hasOptions(mount.SuperOptions, map[bool]string{true: "ro", false: "rw"}[fixed.ReadOnly]) {
				return false
			}
			for _, field := range mount.OptionalFields {
				if strings.HasPrefix(field, "shared:") || strings.HasPrefix(field, "master:") {
					return false
				}
			}
		case "scratch":
			if _, ok := taskWorktreeTmpfs[mount.Destination]; !ok || mount.FSType != "tmpfs" || mount.Source != "tmpfs" || !exactlyOneAccessMode(mount.MountOptions) || !exactlyOneAccessMode(mount.SuperOptions) || !slices.Contains(mount.MountOptions, "rw") || !slices.Contains(mount.SuperOptions, "rw") {
				return false
			}
			if mount.SourceClass != "scratch-tmpfs" || mount.ReceiptID != "" || mount.ReceiptMountID != 0 || mount.ReceiptDevice != 0 || mount.Root != "/" || !validMajorMinor(mount.MajorMinor) || !hasOptions(mount.MountOptions, "nosuid", "nodev") {
				return false
			}
			if mount.Destination != "/tmp" && !slices.Contains(mount.MountOptions, "noexec") {
				return false
			}
			if mount.Destination == "/tmp" && slices.Contains(mount.MountOptions, "noexec") || slices.Contains(mount.MountOptions, "exec") {
				return false
			}
		case "engine-pseudo":
			if _, ok := enginePseudoExact[mount.Destination]; !ok && !thermalMount.MatchString(mount.Destination) {
				return false
			}
			if mount.ReceiptID != "" || mount.ReceiptMountID != 0 || mount.ReceiptDevice != 0 || !validEnginePseudoMount(mount, containerID, dockerDataRoot) {
				return false
			}
		default:
			return false
		}
	}
	for _, fixed := range taskWorktreeMountVector {
		if _, ok := seen[fixed.Destination]; !ok {
			return false
		}
	}
	for destination := range taskWorktreeTmpfs {
		if _, ok := seen[destination]; !ok {
			return false
		}
	}
	for required := range enginePseudoExact {
		_, covered := seen[required]
		_, qualifiedAbsent := absent[required]
		if covered == qualifiedAbsent {
			return false
		}
	}
	return true
}

func validEnginePseudoMount(mount TaskWorktreeRuntimeMount, containerID, dockerDataRoot string) bool {
	if !filepath.IsAbs(mount.Root) || filepath.Clean(mount.Root) != mount.Root || !validMajorMinor(mount.MajorMinor) ||
		!exactlyOneAccessMode(mount.MountOptions) || !exactlyOneAccessMode(mount.SuperOptions) {
		return false
	}
	require := func(root, fsType, source, access string, options ...string) bool {
		return mount.Root == root && mount.FSType == fsType && mount.Source == source &&
			slices.Contains(mount.MountOptions, access) && slices.Contains(mount.SuperOptions, access) && hasOptions(mount.MountOptions, options...)
	}
	switch mount.Destination {
	case "/":
		return mount.SourceClass == "engine" && mount.Root == "/" && mount.FSType == "overlay" && mount.Source == "overlay" && slices.Contains(mount.MountOptions, "ro") && slices.Contains(mount.SuperOptions, "rw")
	case "/proc":
		return mount.SourceClass == "engine" && require("/", "proc", "proc", "rw", "nosuid", "nodev", "noexec")
	case "/dev":
		return mount.SourceClass == "engine" && require("/", "tmpfs", "tmpfs", "rw", "nosuid")
	case "/dev/pts":
		return mount.SourceClass == "engine" && require("/", "devpts", "devpts", "rw", "nosuid", "noexec")
	case "/dev/mqueue":
		return mount.SourceClass == "engine" && require("/", "mqueue", "mqueue", "rw", "nosuid", "nodev", "noexec")
	case "/dev/shm":
		return mount.SourceClass == "engine" && require("/", "tmpfs", "shm", "rw", "nosuid", "nodev", "noexec")
	case "/sys":
		return mount.SourceClass == "engine" && require("/", "sysfs", "sysfs", "ro", "nosuid", "nodev", "noexec")
	case "/sys/fs/cgroup":
		return mount.SourceClass == "engine" && mount.Root == "/" && mount.FSType == "cgroup2" && (mount.Source == "cgroup" || mount.Source == "cgroup2") &&
			slices.Contains(mount.MountOptions, "ro") && slices.Contains(mount.SuperOptions, "rw") && hasOptions(mount.MountOptions, "nosuid", "nodev", "noexec")
	case "/etc/hosts", "/etc/hostname", "/etc/resolv.conf":
		wantRoot := filepath.Join(dockerDataRoot, "containers", containerID, filepath.Base(mount.Destination))
		return mount.SourceClass == "engine-container-file" && isLowerSHA256(containerID) && mount.Root == wantRoot && slices.Contains([]string{"ext4", "xfs", "tmpfs"}, mount.FSType) && (filepath.IsAbs(mount.Source) || mount.Source == "tmpfs") &&
			slices.Contains(mount.MountOptions, "rw") && slices.Contains(mount.SuperOptions, "rw")
	default:
		return mount.SourceClass == "engine" && validEngineCover(mount)
	}
}

func validEngineCover(mount TaskWorktreeRuntimeMount) bool {
	masked := map[string]struct{}{"/proc/interrupts": {}, "/proc/kcore": {}, "/proc/keys": {}, "/proc/latency_stats": {}, "/proc/sched_debug": {}, "/proc/timer_list": {}, "/proc/timer_stats": {}}
	if _, ok := masked[mount.Destination]; ok {
		return mount.Root == "/null" && mount.FSType == "tmpfs" && mount.Source == "tmpfs" && slices.Contains(mount.MountOptions, "rw") && slices.Contains(mount.SuperOptions, "rw") && hasOptions(mount.MountOptions, "nosuid")
	}
	if strings.HasPrefix(mount.Destination, "/proc/") && mount.Destination != "/proc/acpi" && mount.Destination != "/proc/asound" && mount.Destination != "/proc/scsi" {
		return mount.Root == strings.TrimPrefix(mount.Destination, "/proc") && mount.FSType == "proc" && mount.Source == "proc" && slices.Contains(mount.MountOptions, "ro") && slices.Contains(mount.SuperOptions, "rw") && hasOptions(mount.MountOptions, "nosuid", "nodev", "noexec")
	}
	return mount.Root == "/" && mount.FSType == "tmpfs" && mount.Source == "tmpfs" && slices.Contains(mount.MountOptions, "ro") && slices.Contains(mount.SuperOptions, "ro") && hasOptions(mount.MountOptions, "nosuid", "nodev", "noexec")
}

func exactlyOneAccessMode(options []string) bool {
	return slices.Contains(options, "ro") != slices.Contains(options, "rw")
}

func validateQualification(m TaskWorktreeProtectedManifest, q TaskWorktreeHostQualification) error {
	digest, digestErr := canonicalTaskWorktreeQualificationDigest(q)
	if q.Schema != TaskWorktreeHostQualificationSchemaV2 || q.QualificationID == "" || q.DockerEngineID == "" || q.Runtime != m.Runtime ||
		digestErr != nil || !isLowerSHA256(q.Digest) || digest != q.Digest || q.Digest != m.HostQualificationDigest || !isLowerSHA256(q.DockerInfoSHA256) || q.UsernsRemapIdentity == "" || q.ObserverID == "" ||
		q.ProviderProfileID != m.ProviderProfileID || q.ProviderProfileRevision != m.ProviderProfileRevision || q.RequestCoreDigest != m.RequestCoreDigest || q.RuntimeBaselineDigest != m.RuntimeBaseline.Digest || !isLowerSHA256(q.CollectorSHA256) {
		return unsafeTaskState("host qualification does not bind the profile")
	}
	if q.MountSourceMode != TaskWorktreeSourceMagic && q.MountSourceMode != TaskWorktreeSourceCanonical || len(q.ReportedSources) != len(m.Mounts) || len(q.QualifiedAbsent) != len(m.RuntimeBaseline.QualifiedAbsent) {
		return unsafeTaskState("host qualification does not bind source reporting or absence")
	}
	reported := map[string]struct{}{}
	requested := make(map[string]struct{}, len(m.Mounts))
	for _, mount := range m.Mounts {
		requested[mount.Source] = struct{}{}
	}
	for n, mount := range m.Mounts {
		observation := q.ReportedSources[n]
		if observation.ReceiptID != mount.ReceiptID || !filepath.IsAbs(observation.Source) || filepath.Clean(observation.Source) != observation.Source || observation.Source == "/" {
			return unsafeTaskState("host source report differs")
		}
		_, canonicalAliasesMagic := requested[observation.Source]
		if q.MountSourceMode == TaskWorktreeSourceMagic && observation.Source != mount.Source || q.MountSourceMode == TaskWorktreeSourceCanonical && (canonicalAliasesMagic || isProcFDMagicPath(observation.Source)) {
			return unsafeTaskState("host source reporting mode differs")
		}
		if _, duplicate := reported[observation.Source]; duplicate {
			return unsafeTaskState("host source report is reused")
		}
		reported[observation.Source] = struct{}{}
	}
	candidateSet, hostNamespace, hostRoot := "", "", ""
	for n, path := range m.RuntimeBaseline.QualifiedAbsent {
		observed := q.QualifiedAbsent[n]
		if observed.Path != path || observed.ObserverID != q.ObserverID || observed.CandidateSetDigest != taskWorktreeCandidateSetDigest() || observed.HostMountNamespace == "" || observed.HostRootReceiptID == "" || observed.LstatState != "absent" || observed.Errno != "ENOENT" {
			return unsafeTaskState("host absence qualification differs")
		}
		if n == 0 {
			candidateSet, hostNamespace, hostRoot = observed.CandidateSetDigest, observed.HostMountNamespace, observed.HostRootReceiptID
		}
		if observed.CandidateSetDigest != candidateSet || observed.HostMountNamespace != hostNamespace || observed.HostRootReceiptID != hostRoot {
			return unsafeTaskState("host absence provenance is mixed")
		}
	}
	return nil
}

func verifyRuntimeIdentity(m TaskWorktreeProtectedManifest, identity TaskWorktreeRuntimeIdentity, qualification TaskWorktreeHostQualification) error {
	observerID := qualification.ObserverID
	uidOutside, uidOK := canonicalRemappedID(identity.UIDMap, TaskWorktreeUID)
	gidOutside, gidOK := canonicalRemappedID(identity.GIDMap, TaskWorktreeGID)
	if identity.ContainerID != m.ContainerID || identity.InitPID <= 0 || identity.DockerInspectPID != identity.InitPID || identity.InitDevice == 0 || identity.InitInode == 0 ||
		identity.InitSHA256 != m.TiniSHA256 || identity.EffectiveUID != TaskWorktreeUID || identity.EffectiveGID != TaskWorktreeGID ||
		identity.ProcRootReceiptID == "" || !isLowerSHA256(identity.CollectorExecID) || identity.CollectorExecPID <= 0 || identity.CollectorExecPID == identity.InitPID || identity.CollectorExecStartTime == 0 || identity.CollectorSHA256 != qualification.CollectorSHA256 ||
		!uidOK || !gidOK || uidOutside == uint64(TaskWorktreeUID) || gidOutside == uint64(TaskWorktreeGID) {
		return unsafeTaskState("container init or user namespace identity differs")
	}
	if len(identity.Processes) != 2 || identity.Processes[0].PID != identity.InitPID || identity.Processes[0].PPID != 0 || identity.Processes[0].ExecutableSHA256 != m.TiniSHA256 || identity.Processes[0].Role != "init" ||
		identity.Processes[1].PPID != identity.InitPID || identity.Processes[1].PID <= identity.InitPID || identity.Processes[1].ExecutableSHA256 != m.LatchSHA256 || identity.Processes[1].Role != "latch" || identity.Processes[1].State != "T" ||
		!slices.Equal(identity.Processes[0].Cmdline, append([]string{"/usr/bin/tini", "-g", "--", TaskWorktreeLatchPath}, m.ProviderArgv...)) || !slices.Equal(identity.Processes[1].Cmdline, append([]string{TaskWorktreeLatchPath}, m.ProviderArgv...)) {
		return unsafeTaskState("pre-acceptance process tree differs")
	}
	if identity.CollectorExecPID == identity.Processes[1].PID {
		return unsafeTaskState("collector process aliases the inert latch")
	}
	if identity.ProviderExecutable != m.ProviderExecutable {
		return unsafeTaskState("provider executable identity differs")
	}
	if len(identity.Sources) != len(m.Receipts) {
		return unsafeTaskState("source observation count differs")
	}
	for n, receipt := range m.Receipts {
		observed, mount := identity.Sources[n], m.Mounts[n]
		if observed.ReceiptID != receipt.ReceiptID || observed.DockerSource != expectedInspectSource(qualification, n) || observed.MountID != receipt.MountID ||
			observed.Device != receipt.Device || observed.Inode != receipt.Inode || observed.Kind != receipt.Kind || observed.OwnerUID != receipt.OwnerUID ||
			observed.OwnerGID != receipt.OwnerGID || observed.HeldFDGeneration != receipt.HeldFDGeneration || !observed.Readable || observed.Writable == mount.ReadOnly {
			return unsafeTaskState("source observation differs at %s", receipt.Role)
		}
		if !mount.ReadOnly && (uint64(observed.OwnerUID) != uidOutside || uint64(observed.OwnerGID) != gidOutside) {
			return unsafeTaskState("writable source ownership does not match remapped identity")
		}
	}
	if len(identity.QualifiedAbsent) != len(m.RuntimeBaseline.QualifiedAbsent) {
		return unsafeTaskState("container absence evidence is incomplete")
	}
	for n, path := range m.RuntimeBaseline.QualifiedAbsent {
		observed := identity.QualifiedAbsent[n]
		if observed.Path != path || observed.QualificationID != qualification.QualificationID || observed.ObserverID != observerID || observed.ContainerID != m.ContainerID || observed.InitPID != identity.InitPID || observed.ProcRootReceiptID != identity.ProcRootReceiptID || observed.LstatState != "absent" || observed.Errno != "ENOENT" {
			return unsafeTaskState("container absence evidence differs")
		}
	}
	if err := verifyTaskWorktreeEffectiveEnvironments(m, identity, qualification); err != nil {
		return err
	}
	if len(identity.Endpoints) != len(taskWorktreeEndpointPaths) || len(identity.InheritedFDs) != 3 || !identity.OpenUnixSockets.Complete || identity.OpenUnixSockets.ObserverID != observerID || identity.OpenUnixSockets.ContainerID != m.ContainerID || identity.OpenUnixSockets.InitPID != identity.InitPID || len(identity.OpenUnixSockets.Filesystem) != 0 || len(identity.OpenUnixSockets.Abstract) != 0 {
		return unsafeTaskState("endpoint evidence is incomplete")
	}
	for n, path := range taskWorktreeEndpointPaths {
		e := identity.Endpoints[n]
		if e.ObserverID != identity.OpenUnixSockets.ObserverID || e.ContainerID != m.ContainerID || e.InitPID != identity.InitPID || e.Path != path || e.LstatState != "absent" || e.FileType != "none" || e.ConnectResult != "ENOENT" {
			return unsafeTaskState("privileged endpoint is reachable or unobserved")
		}
	}
	for n, fd := range identity.InheritedFDs {
		if fd.Number != n || fd.ReceiptID != m.Console.ReceiptID || fd.Kind != m.Console.Kind || fd.Target != m.Console.Target || fd.MajorMinor != m.Console.MajorMinor || fd.Device != m.Console.Device || fd.Inode != m.Console.Inode {
			return unsafeTaskState("inherited descriptor differs")
		}
	}
	return nil
}

func verifyTaskWorktreeEffectiveEnvironments(manifest TaskWorktreeProtectedManifest, identity TaskWorktreeRuntimeIdentity, qualification TaskWorktreeHostQualification) error {
	if err := validateManifest(manifest); err != nil {
		return err
	}
	if err := validateQualification(manifest, qualification); err != nil {
		return err
	}
	if !validRunID(manifest.RunID) || manifest.Hostname != "ndq-"+manifest.RunID {
		return unsafeTaskState("effective environment authority is invalid")
	}
	expected := append(CanonicalTaskWorktreeConfigEnv(), "HOSTNAME="+manifest.Hostname)
	slices.SortFunc(expected, compareEnvironmentNames)
	if len(identity.Environments) != 2 {
		return unsafeTaskState("effective environment evidence is incomplete")
	}
	for n, role := range []string{"initial", "exec"} {
		observed := identity.Environments[n]
		wrongPID := role == "initial" && observed.ProcessPID != identity.InitPID || role == "exec" && (observed.ProcessPID <= 0 || observed.ProcessPID == identity.InitPID)
		wrongExec := role == "initial" && (observed.ExecID != "" || observed.ProcessStartTime != 0 || observed.ExecutableSHA256 != "") ||
			role == "exec" && (observed.ExecID != identity.CollectorExecID || observed.ProcessPID != identity.CollectorExecPID || observed.ProcessStartTime != identity.CollectorExecStartTime || observed.ExecutableSHA256 != identity.CollectorSHA256)
		if observed.Role != role || observed.QualificationID != qualification.QualificationID || observed.ObserverID != qualification.ObserverID || observed.ContainerID != manifest.ContainerID || observed.InitPID != identity.InitPID || !observed.Complete || wrongPID ||
			wrongExec || !validCanonicalEnvironment(observed.Entries) || !slices.Equal(observed.Entries, expected) {
			return unsafeTaskState("%s effective environment differs", role)
		}
	}
	return nil
}

func parseTaskMountInfo(raw []byte) ([]TaskWorktreeRuntimeMount, error) {
	var mounts []TaskWorktreeRuntimeMount
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), maxTaskMountInfoBytes)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		separator := slices.Index(fields, "-")
		if separator < 6 || len(fields) < separator+4 {
			return nil, errors.New("malformed mountinfo")
		}
		mounts = append(mounts, TaskWorktreeRuntimeMount{MajorMinor: fields[2], Root: decodeMountInfo(fields[3]), Destination: decodeMountInfo(fields[4]), MountOptions: splitOptions(fields[5]), OptionalFields: append([]string(nil), fields[6:separator]...), FSType: fields[separator+1], Source: decodeMountInfo(fields[separator+2]), SuperOptions: splitOptions(fields[separator+3])})
	}
	if err := scanner.Err(); err != nil || len(mounts) == 0 {
		return nil, errors.New("empty or unreadable mountinfo")
	}
	return mounts, nil
}

func decodeMountInfo(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+3 < len(value) {
			if decoded, ok := map[string]byte{"040": ' ', "011": '\t', "012": '\n', "134": '\\'}[value[i+1:i+4]]; ok {
				out.WriteByte(decoded)
				i += 3
				continue
			}
		}
		out.WriteByte(value[i])
	}
	return out.String()
}

func runtimeMountsEqual(actual, expected []TaskWorktreeRuntimeMount) bool {
	if len(actual) != len(expected) {
		return false
	}
	for n := range actual {
		copy := expected[n]
		copy.Class, copy.SourceClass, copy.ReceiptID = "", "", ""
		copy.ReceiptMountID, copy.ReceiptDevice = 0, 0
		if !reflect.DeepEqual(actual[n], copy) {
			return false
		}
	}
	return true
}

func parseStatus(raw []byte) map[string]string {
	status := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		if key, value, ok := strings.Cut(scanner.Text(), ":"); ok {
			status[key] = strings.TrimSpace(value)
		}
	}
	return status
}

func fixedMountAt(destination string) (TaskWorktreeMountRole, bool) {
	for _, fixed := range taskWorktreeMountVector {
		if fixed.Destination == destination {
			return fixed, true
		}
	}
	return TaskWorktreeMountRole{}, false
}

func expectedInspectSource(qualification TaskWorktreeHostQualification, index int) string {
	if index < 0 || index >= len(qualification.ReportedSources) {
		return ""
	}
	return qualification.ReportedSources[index].Source
}

func isProcFDMagicPath(path string) bool {
	// Canonical backing paths are protected host paths, never procfs spellings.
	// Rejecting the whole procfs namespace also closes self, thread-self, and
	// task aliases without trying to duplicate the kernel's magic-link grammar.
	return path == "/proc" || strings.HasPrefix(path, "/proc/")
}

func taskWorktreeCandidateSetDigest() string {
	digest := sha256.New()
	_, _ = io.WriteString(digest, "noisedeck.task-worktree-cover-candidates.v1\n")
	for _, path := range append(slices.Clone(taskWorktreeMaskedPaths), taskWorktreeReadonlyPaths...) {
		_, _ = io.WriteString(digest, path+"\n")
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func CanonicalTaskWorktreeCandidateSetDigest() string {
	return taskWorktreeCandidateSetDigest()
}

func canonicalRemappedID(mappings []TaskWorktreeIDMap, inside uint32) (uint64, bool) {
	if len(mappings) != 1 || mappings[0].Inside != 0 || mappings[0].Outside < 65536 || mappings[0].Length != 65536 || uint64(inside) >= mappings[0].Length || mappings[0].Outside > uint64(^uint32(0))-mappings[0].Length {
		return 0, false
	}
	return mappings[0].Outside + uint64(inside), true
}

func validImageID(value string) bool {
	return strings.HasPrefix(value, "sha256:") && isLowerSHA256(strings.TrimPrefix(value, "sha256:"))
}
func validTaskNetwork(mode string) bool {
	return mode != "" && mode != "host" && mode != "none" && mode != "default" && mode != "bridge" && !strings.HasPrefix(mode, "container:")
}
func validDockerDataRoot(root string) bool {
	return filepath.IsAbs(root) && filepath.Clean(root) == root && root != "/" && root != "/project" && root != "/project.git" && !strings.HasPrefix(root, "/project/") && !strings.HasPrefix(root, "/project.git/")
}
func validProviderExecutable(executable TaskWorktreeExecutableReceipt) bool {
	if !strings.HasPrefix(executable.Path, "/usr/local/bin/") && !strings.HasPrefix(executable.Path, "/opt/noisedeck/bin/") {
		return false
	}
	if !filepath.IsAbs(executable.Path) || filepath.Clean(executable.Path) != executable.Path || !isLowerSHA256(executable.SHA256) || executable.Kind != "regular" || executable.MountID == 0 || executable.Device == 0 || executable.Inode == 0 {
		return false
	}
	for _, prefix := range []string{"/project", "/project.git", "/home", "/tmp", "/run"} {
		if executable.Path == prefix || strings.HasPrefix(executable.Path, prefix+"/") {
			return false
		}
	}
	return true
}
func validConsoleReceipt(console TaskWorktreeConsoleReceipt) bool {
	minorText := strings.TrimPrefix(console.Target, "/dev/pts/")
	major, minor, ok := strings.Cut(console.MajorMinor, ":")
	if console.ReceiptID == "" || console.Kind != "character" || !strings.HasPrefix(console.Target, "/dev/pts/") || minorText == "" || !ok || major != "136" || minor != minorText || console.Device == 0 || console.Inode == 0 {
		return false
	}
	_, targetErr := strconv.ParseUint(minorText, 10, 32)
	_, minorErr := strconv.ParseUint(minor, 10, 32)
	return targetErr == nil && minorErr == nil
}
func validCanonicalEnvironment(environment []string) bool {
	seen := map[string]struct{}{}
	previous := ""
	for _, entry := range environment {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "" || previous != "" && previous >= name {
			return false
		}
		if _, dup := seen[name]; dup {
			return false
		}
		seen[name] = struct{}{}
		previous = name
	}
	return len(seen) == 6
}

func compareEnvironmentNames(left, right string) int {
	leftName, _, _ := strings.Cut(left, "=")
	rightName, _, _ := strings.Cut(right, "=")
	return strings.Compare(leftName, rightName)
}

func validRunID(runID string) bool {
	if len(runID) != 32 {
		return false
	}
	for _, char := range runID {
		if char < '0' || char > '9' && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validQualifiedAbsentPaths(paths []string) bool {
	allowed := append(CanonicalTaskWorktreeMaskedPaths(), taskWorktreeReadonlyPaths...)
	position := -1
	for _, path := range paths {
		index := slices.Index(allowed, path)
		if index <= position {
			return false
		}
		position = index
	}
	return true
}
func mountModeMatches(mode string, ro bool) bool {
	options := strings.Split(mode, ",")
	return slices.Contains(options, "ro") == ro && slices.Contains(options, "rw") != ro
}
func splitOptions(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}
func hasOptions(options []string, wanted ...string) bool {
	for _, want := range wanted {
		if !slices.Contains(options, want) {
			return false
		}
	}
	return true
}
func validMajorMinor(value string) bool {
	_, ok := majorMinorDevice(value)
	return ok
}
func majorMinorDevice(value string) (uint64, bool) {
	major, minor, ok := strings.Cut(value, ":")
	if !ok {
		return 0, false
	}
	majorNumber, e1 := strconv.ParseUint(major, 10, 32)
	minorNumber, e2 := strconv.ParseUint(minor, 10, 32)
	if e1 != nil || e2 != nil {
		return 0, false
	}
	if major != strconv.FormatUint(majorNumber, 10) || minor != strconv.FormatUint(minorNumber, 10) {
		return 0, false
	}
	return majorNumber<<32 | minorNumber, true
}
func equalFoldExact(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for n := range got {
		if !strings.EqualFold(got[n], want[n]) {
			return false
		}
	}
	return true
}
func equalStringMap(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		if got[key] != value {
			return false
		}
	}
	return true
}
func cloneMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
func unsafeTaskState(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrTaskWorktreeEffectiveState, fmt.Sprintf(format, args...))
}
