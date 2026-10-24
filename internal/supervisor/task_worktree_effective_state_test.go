package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestTaskWorktreeExactInspectProfile(t *testing.T) {
	manifest := validManifest(t)
	qualification := validQualification(manifest)
	if err := VerifyTaskWorktreeDockerInspect(marshalInspect(t, validInspect(manifest, qualification)), manifest, qualification); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalTaskWorktreePolicyVectorsAreExactAndCopied(t *testing.T) {
	wantEnv := []string{"HOME=/home/agent", "PATH=/usr/local/bin:/usr/bin:/bin", "NODE_VERSION=26.12.1", "YARN_VERSION=1.22.22", "TERM=xterm-256color"}
	wantMasked := []string{"/proc/acpi", "/proc/asound", "/proc/interrupts", "/proc/kcore", "/proc/keys", "/proc/latency_stats", "/proc/sched_debug", "/proc/scsi", "/proc/timer_list", "/proc/timer_stats", "/sys/devices/virtual/powercap", "/sys/firmware"}
	wantReadonly := []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"}
	if !slices.Equal(CanonicalTaskWorktreeConfigEnv(), wantEnv) || !slices.Equal(CanonicalTaskWorktreeMaskedPaths(), wantMasked) || !slices.Equal(CanonicalTaskWorktreeReadonlyPaths(), wantReadonly) || !reflect.DeepEqual(CanonicalTaskWorktreeMountRoles(), goldenMountRoles()) {
		t.Fatal("canonical task-worktree policy differs from the ratified literals")
	}
	environment, masked, readonly, mounts := CanonicalTaskWorktreeConfigEnv(), CanonicalTaskWorktreeMaskedPaths(), CanonicalTaskWorktreeReadonlyPaths(), CanonicalTaskWorktreeMountRoles()
	environment[0], masked[0], readonly[0], mounts[0].Role = "changed", "changed", "changed", "changed"
	if CanonicalTaskWorktreeConfigEnv()[0] != wantEnv[0] || CanonicalTaskWorktreeMaskedPaths()[0] != wantMasked[0] || CanonicalTaskWorktreeReadonlyPaths()[0] != wantReadonly[0] || CanonicalTaskWorktreeMountRoles()[0].Role != goldenMountRoles()[0].Role {
		t.Fatal("canonical policy constructor exposed mutable package state")
	}
	m := validManifest(t)
	labels := canonicalTaskWorktreeLabels(m)
	wantLabels := map[string]string{"com.noisedeck.phase-a.schema": "noisedeck.task-worktree-phase-a.v1", "com.noisedeck.phase-a.run-id": m.RunID, "com.noisedeck.phase-a.profile-revision": m.ProviderProfileRevision, "com.noisedeck.phase-a.qualifier-build": m.QualifierBuildDigest, "com.noisedeck.phase-a.request-core": m.RequestCoreDigest}
	if !maps.Equal(labels, wantLabels) {
		t.Fatalf("canonical labels differ: %#v", labels)
	}
	labels["com.noisedeck.phase-a.schema"] = "changed"
	if canonicalTaskWorktreeLabels(m)["com.noisedeck.phase-a.schema"] != "noisedeck.task-worktree-phase-a.v1" {
		t.Fatal("canonical label constructor exposed mutable state")
	}
}

func TestEveryCanonicalMaskAndReadonlyPathIsMandatory(t *testing.T) {
	for _, vector := range []struct {
		name  string
		paths []string
		set   func(*TaskWorktreeProtectedManifest, []string)
	}{
		{"masked", CanonicalTaskWorktreeMaskedPaths(), func(m *TaskWorktreeProtectedManifest, paths []string) { m.MaskedPaths = paths }},
		{"readonly", CanonicalTaskWorktreeReadonlyPaths(), func(m *TaskWorktreeProtectedManifest, paths []string) { m.ReadonlyPaths = paths }},
	} {
		for n, path := range vector.paths {
			t.Run(vector.name+path, func(t *testing.T) {
				m := validManifest(t)
				changed := slices.Delete(slices.Clone(vector.paths), n, n+1)
				vector.set(&m, changed)
				sealManifest(t, &m)
				q := validQualification(m)
				requireUnsafe(t, VerifyTaskWorktreeDockerInspect(marshalInspect(t, validInspect(m, q)), m, q))
			})
		}
	}
}

func TestEveryCanonicalLabelIsMandatoryAndExact(t *testing.T) {
	m := validManifest(t)
	q := validQualification(m)
	for _, key := range []string{"com.noisedeck.phase-a.schema", "com.noisedeck.phase-a.run-id", "com.noisedeck.phase-a.profile-revision", "com.noisedeck.phase-a.qualifier-build", "com.noisedeck.phase-a.request-core"} {
		for _, change := range []string{"delete", "change"} {
			t.Run(key+"/"+change, func(t *testing.T) {
				i := validInspect(m, q)
				labels := nested(i, "Config")["Labels"].(map[string]string)
				if change == "delete" {
					delete(labels, key)
				} else {
					labels[key] = "changed"
				}
				requireUnsafe(t, VerifyTaskWorktreeDockerInspect(marshalInspect(t, i), m, q))
			})
		}
	}
}

func TestTaskWorktreeInspectRejectsEveryActualExpansion(t *testing.T) {
	manifest := validManifest(t)
	qualification := validQualification(manifest)
	cases := map[string]func(map[string]any){
		"container":     func(i map[string]any) { i["Id"] = strings.Repeat("9", 64) },
		"image":         func(i map[string]any) { i["Image"] = "sha256:" + strings.Repeat("9", 64) },
		"path":          func(i map[string]any) { i["Path"] = "/bin/sh" },
		"args":          func(i map[string]any) { i["Args"] = []string{"-c", "true"} },
		"restart count": func(i map[string]any) { i["RestartCount"] = 1 },
		"user":          func(i map[string]any) { nested(i, "Config")["User"] = "0:0" },
		"hostname":      func(i map[string]any) { nested(i, "Config")["Hostname"] = "ndq-" + strings.Repeat("9", 32) },
		"missing label": func(i map[string]any) {
			delete(nested(i, "Config")["Labels"].(map[string]string), "com.noisedeck.phase-a.run-id")
		},
		"changed label": func(i map[string]any) {
			nested(i, "Config")["Labels"].(map[string]string)["com.noisedeck.phase-a.profile-revision"] = "other"
		},
		"extra label": func(i map[string]any) { nested(i, "Config")["Labels"].(map[string]string)["other"] = "value" },
		"latch":       func(i map[string]any) { nested(i, "Config")["Entrypoint"] = []string{"/bin/sh"} },
		"argv":        func(i map[string]any) { nested(i, "Config")["Cmd"] = []string{"/bin/true"} },
		"env":         func(i map[string]any) { nested(i, "Config")["Env"] = []string{"PATH=/project"} },
		"cwd":         func(i map[string]any) { nested(i, "Config")["WorkingDir"] = "/" },
		"stop signal": func(i map[string]any) { nested(i, "Config")["StopSignal"] = "SIGKILL" },
		"attach":      func(i map[string]any) { nested(i, "Config")["AttachStdout"] = false },
		"stdin":       func(i map[string]any) { nested(i, "Config")["OpenStdin"] = false },
		"tty":         func(i map[string]any) { nested(i, "Config")["Tty"] = false },
		"port": func(i map[string]any) {
			nested(i, "Config")["ExposedPorts"] = map[string]any{"80/tcp": map[string]any{}}
		},
		"health":      func(i map[string]any) { nested(nested(i, "Config"), "Healthcheck")["Test"] = []string{"CMD", "true"} },
		"not running": func(i map[string]any) { nested(i, "State")["Running"] = false },
		"restarting":  func(i map[string]any) { nested(i, "State")["Restarting"] = true },
		"state error": func(i map[string]any) { nested(i, "State")["Error"] = "boom" },
		"rootfs":      func(i map[string]any) { nested(i, "HostConfig")["ReadonlyRootfs"] = false },
		"privileged":  func(i map[string]any) { nested(i, "HostConfig")["Privileged"] = true },
		"autoremove":  func(i map[string]any) { nested(i, "HostConfig")["AutoRemove"] = true },
		"docker init": func(i map[string]any) { nested(i, "HostConfig")["Init"] = true },
		"publish":     func(i map[string]any) { nested(i, "HostConfig")["PublishAllPorts"] = true },
		"cap add":     func(i map[string]any) { nested(i, "HostConfig")["CapAdd"] = []string{"SYS_ADMIN"} },
		"cap drop":    func(i map[string]any) { nested(i, "HostConfig")["CapDrop"] = []string{"NET_RAW"} },
		"security": func(i map[string]any) {
			nested(i, "HostConfig")["SecurityOpt"] = []string{"no-new-privileges:true", "seccomp=unconfined"}
		},
		"runtime":              func(i map[string]any) { nested(i, "HostConfig")["Runtime"] = "other" },
		"network":              func(i map[string]any) { nested(i, "HostConfig")["NetworkMode"] = "host" },
		"pid":                  func(i map[string]any) { nested(i, "HostConfig")["PidMode"] = "host" },
		"ipc":                  func(i map[string]any) { nested(i, "HostConfig")["IpcMode"] = "shareable" },
		"userns":               func(i map[string]any) { nested(i, "HostConfig")["UsernsMode"] = "host" },
		"cgroupns":             func(i map[string]any) { nested(i, "HostConfig")["CgroupnsMode"] = "host" },
		"cgroup parent":        func(i map[string]any) { nested(i, "HostConfig")["CgroupParent"] = "/other" },
		"uts":                  func(i map[string]any) { nested(i, "HostConfig")["UTSMode"] = "host" },
		"isolation":            func(i map[string]any) { nested(i, "HostConfig")["Isolation"] = "hyperv" },
		"stop timeout":         func(i map[string]any) { nested(i, "Config")["StopTimeout"] = 5 },
		"missing stop timeout": func(i map[string]any) { delete(nested(i, "Config"), "StopTimeout") },
		"restart":              func(i map[string]any) { nested(nested(i, "HostConfig"), "RestartPolicy")["Name"] = "always" },
		"logging":              func(i map[string]any) { nested(nested(i, "HostConfig"), "LogConfig")["Type"] = "json-file" },
		"group":                func(i map[string]any) { nested(i, "HostConfig")["GroupAdd"] = []string{"0"} },
		"volume from":          func(i map[string]any) { nested(i, "HostConfig")["VolumesFrom"] = []string{"other"} },
		"extra host":           func(i map[string]any) { nested(i, "HostConfig")["ExtraHosts"] = []string{"host:127.0.0.1"} },
		"device cgroup":        func(i map[string]any) { nested(i, "HostConfig")["DeviceCgroupRules"] = []string{"c 1:3 rwm"} },
		"container id file":    func(i map[string]any) { nested(i, "HostConfig")["ContainerIDFile"] = "/tmp/id" },
		"link":                 func(i map[string]any) { nested(i, "HostConfig")["Links"] = []string{"other"} },
		"network identity": func(i map[string]any) {
			nested(nested(i, "NetworkSettings"), "Networks")["other"] = map[string]any{"NetworkID": strings.Repeat("9", 64)}
		},
		"network id": func(i map[string]any) {
			nested(nested(nested(i, "NetworkSettings"), "Networks"), manifest.NetworkMode)["NetworkID"] = strings.Repeat("9", 64)
		},
		"device": func(i map[string]any) {
			nested(i, "HostConfig")["Devices"] = []any{map[string]any{"PathOnHost": "/dev/kvm"}}
		},
		"device request": func(i map[string]any) { nested(i, "HostConfig")["DeviceRequests"] = []any{map[string]any{"Count": -1}} },
		"sysctl":         func(i map[string]any) { nested(i, "HostConfig")["Sysctls"] = map[string]string{"x": "y"} },
		"tmpfs":          func(i map[string]any) { nested(i, "HostConfig")["Tmpfs"].(map[string]string)["/other"] = "rw" },
		"masked":         func(i map[string]any) { nested(i, "HostConfig")["MaskedPaths"] = []string{} },
		"readonly":       func(i map[string]any) { nested(i, "HostConfig")["ReadonlyPaths"] = []string{} },
		"port binding":   func(i map[string]any) { nested(i, "HostConfig")["PortBindings"] = map[string]any{"80/tcp": []any{}} },
		"mount source":   func(i map[string]any) { i["Mounts"].([]map[string]any)[0]["Source"] = "/other" },
		"mount mode": func(i map[string]any) {
			i["Mounts"].([]map[string]any)[1]["Mode"] = "rw"
			i["Mounts"].([]map[string]any)[1]["RW"] = true
		},
		"mount propagation": func(i map[string]any) { i["Mounts"].([]map[string]any)[0]["Propagation"] = "rshared" },
		"extra mount": func(i map[string]any) {
			i["Mounts"] = append(i["Mounts"].([]map[string]any), map[string]any{"Type": "bind", "Source": "/", "Destination": "/host", "Mode": "rw", "RW": true, "Propagation": "rprivate"})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			inspect := validInspect(manifest, qualification)
			mutate(inspect)
			requireUnsafe(t, VerifyTaskWorktreeDockerInspect(marshalInspect(t, inspect), manifest, qualification))
		})
	}
}

func TestForbiddenExpectedProfileStillFailsWhenInspectMatches(t *testing.T) {
	for name, mutate := range map[string]func(*TaskWorktreeProtectedManifest, map[string]any){
		"wrong user": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.User = "2000:2000"
			nested(i, "Config")["User"] = m.User
		},
		"relative provider": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.ProviderArgv = []string{"provider"}
			nested(i, "Config")["Cmd"] = m.ProviderArgv
			i["Args"] = append([]string{"-g", "--", TaskWorktreeLatchPath}, m.ProviderArgv...)
		},
		"writable provider": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.ProviderArgv[0] = "/project/provider"
			m.ProviderExecutable.Path = m.ProviderArgv[0]
			nested(i, "Config")["Cmd"] = m.ProviderArgv
			i["Args"] = append([]string{"-g", "--", TaskWorktreeLatchPath}, m.ProviderArgv...)
		},
		"non-image provider": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.ProviderArgv[0] = "/dev/shm/provider"
			m.ProviderExecutable.Path = m.ProviderArgv[0]
			nested(i, "Config")["Cmd"] = m.ProviderArgv
			i["Args"] = append([]string{"-g", "--", TaskWorktreeLatchPath}, m.ProviderArgv...)
		},
		"loader env": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.ConfigEnv = append(m.ConfigEnv, "LD_PRELOAD=/tmp/x")
			nested(i, "Config")["Env"] = m.ConfigEnv
		},
		"hostname authority": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.Hostname = "ndq-" + strings.Repeat("9", 32)
			nested(i, "Config")["Hostname"] = m.Hostname
		},
		"qualifier build label": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.QualifierBuildDigest = "not-a-digest"
			nested(i, "Config")["Labels"] = canonicalTaskWorktreeLabels(*m)
		},
		"missing provider profile revision": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.ProviderProfileRevision = ""
			nested(i, "Config")["Labels"] = canonicalTaskWorktreeLabels(*m)
		},
		"wrong runtime": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.Runtime = "other"
			nested(i, "HostConfig")["Runtime"] = m.Runtime
		},
		"host network": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.NetworkMode = "host"
			nested(i, "HostConfig")["NetworkMode"] = m.NetworkMode
		},
		"writable config": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.Mounts[4].ReadOnly = false
			i["Mounts"].([]map[string]any)[4]["Mode"] = "rw"
			i["Mounts"].([]map[string]any)[4]["RW"] = true
		},
		"source reuse": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.Receipts[1].MountID = m.Receipts[0].MountID
			m.Receipts[1].Device = m.Receipts[0].Device
			m.Receipts[1].Inode = m.Receipts[0].Inode
		},
		"arbitrary baseline": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.RuntimeBaseline.Mounts = append(m.RuntimeBaseline.Mounts, TaskWorktreeRuntimeMount{Class: "engine-pseudo", Destination: "/workspace", MountOptions: []string{"rw"}})
		},
		"console receipt": func(m *TaskWorktreeProtectedManifest, _ map[string]any) {
			m.Console.Target = "/dev/pts/other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := validManifest(t)
			qualification := validQualification(m)
			inspect := validInspect(m, qualification)
			mutate(&m, inspect)
			sealManifest(t, &m)
			qualification = validQualification(m)
			requireUnsafe(t, VerifyTaskWorktreeDockerInspect(marshalInspect(t, inspect), m, qualification))
		})
	}
}

func TestRuntimeBaselineRejectsPairedUnsafeExpectedAndObserved(t *testing.T) {
	type mutation struct {
		destination string
		mutate      func(*TaskWorktreeRuntimeMount)
	}
	for name, change := range map[string]mutation{
		"source class":             {"/project", func(m *TaskWorktreeRuntimeMount) { m.SourceClass = "caller" }},
		"filesystem":               {"/project", func(m *TaskWorktreeRuntimeMount) { m.FSType = "" }},
		"root":                     {"/project", func(m *TaskWorktreeRuntimeMount) { m.Root = "relative" }},
		"options":                  {"/project", func(m *TaskWorktreeRuntimeMount) { m.MountOptions = []string{"rw"} }},
		"device":                   {"/project", func(m *TaskWorktreeRuntimeMount) { m.ReceiptDevice++ }},
		"mount device":             {"/project", func(m *TaskWorktreeRuntimeMount) { m.MajorMinor = "8:99" }},
		"engine source":            {"/dev/pts", func(m *TaskWorktreeRuntimeMount) { m.Source = "tmpfs" }},
		"engine root":              {"/etc/hosts", func(m *TaskWorktreeRuntimeMount) { m.Root = "/docker/containers/other/hosts" }},
		"engine filesystem":        {"/dev/mqueue", func(m *TaskWorktreeRuntimeMount) { m.FSType = "tmpfs" }},
		"engine options":           {"/dev/shm", func(m *TaskWorktreeRuntimeMount) { m.MountOptions = []string{"ro", "nosuid", "nodev", "noexec"} }},
		"cgroup superblock access": {"/sys/fs/cgroup", func(m *TaskWorktreeRuntimeMount) { m.SuperOptions = []string{"ro"} }},
		"engine device alias":      {"/dev", func(m *TaskWorktreeRuntimeMount) { m.ReceiptDevice = 1 }},
		"scratch mount options":    {"/home/agent", func(m *TaskWorktreeRuntimeMount) { m.MountOptions = []string{"ro", "nosuid", "nodev", "noexec"} }},
		"scratch super options":    {"/home/agent", func(m *TaskWorktreeRuntimeMount) { m.SuperOptions = []string{"ro"} }},
	} {
		t.Run(name, func(t *testing.T) {
			manifest := validManifest(t)
			change.mutate(runtimeMountAt(t, &manifest.RuntimeBaseline, change.destination))
			sealManifest(t, &manifest)
			q := validQualification(manifest)
			identity := validRuntimeIdentity(manifest, q)
			requireUnsafe(t, VerifyTaskWorktreeRuntimeState(mountInfoFromBaseline(manifest.RuntimeBaseline), validStatus(), manifest, q, identity))
		})
	}
}

func TestCanonicalRequestVectorsRejectPairedExpectedAndInspectMutation(t *testing.T) {
	for name, mutate := range map[string]func(*TaskWorktreeProtectedManifest, map[string]any){
		"config env": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.ConfigEnv[0] = "HOME=/tmp"
			nested(i, "Config")["Env"] = m.ConfigEnv
		},
		"masked paths": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.MaskedPaths = slices.Delete(m.MaskedPaths, 1, 2)
			nested(i, "HostConfig")["MaskedPaths"] = m.MaskedPaths
		},
		"masked order": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.MaskedPaths[0], m.MaskedPaths[1] = m.MaskedPaths[1], m.MaskedPaths[0]
			nested(i, "HostConfig")["MaskedPaths"] = m.MaskedPaths
		},
		"readonly paths": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.ReadonlyPaths = append(m.ReadonlyPaths, "/other")
			nested(i, "HostConfig")["ReadonlyPaths"] = m.ReadonlyPaths
		},
		"broad objects info": func(m *TaskWorktreeProtectedManifest, i map[string]any) {
			m.Mounts = slices.Delete(m.Mounts, 7, 10)
			m.Receipts = slices.Delete(m.Receipts, 7, 10)
			m.Mounts = append(m.Mounts[:7], append([]TaskWorktreeMountExpectation{{Role: "git_objects_info", Type: "bind", Source: "/proc/700/fd/17", ReceiptID: "receipt-07", Destination: "/project.git/objects/info", Propagation: "rprivate", ReadOnly: true}}, m.Mounts[7:]...)...)
			nested(i, "HostConfig")["MaskedPaths"] = m.MaskedPaths
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := validManifest(t)
			q := validQualification(m)
			i := validInspect(m, q)
			mutate(&m, i)
			sealManifest(t, &m)
			q = validQualification(m)
			i = validInspect(m, q)
			requireUnsafe(t, VerifyTaskWorktreeDockerInspect(marshalInspect(t, i), m, q))
		})
	}
}

func TestNestedObjectPinsRejectPairedUnsafeAuthority(t *testing.T) {
	for name, mutate := range map[string]func(*TaskWorktreeProtectedManifest){
		"objects read-only": func(m *TaskWorktreeProtectedManifest) {
			m.Mounts[7].ReadOnly = true
			mount := runtimeMountAt(t, &m.RuntimeBaseline, m.Mounts[7].Destination)
			mount.MountOptions[0], mount.SuperOptions[0] = "ro", "ro"
		},
		"objects-info read-only": func(m *TaskWorktreeProtectedManifest) {
			m.Mounts[8].ReadOnly = true
			mount := runtimeMountAt(t, &m.RuntimeBaseline, m.Mounts[8].Destination)
			mount.MountOptions[0], mount.SuperOptions[0] = "ro", "ro"
		},
		"alternates writable": func(m *TaskWorktreeProtectedManifest) {
			m.Mounts[9].ReadOnly = false
			mount := runtimeMountAt(t, &m.RuntimeBaseline, m.Mounts[9].Destination)
			mount.MountOptions[0], mount.SuperOptions[0] = "rw", "rw"
		},
		"alternates directory": func(m *TaskWorktreeProtectedManifest) { m.Receipts[9].Kind = "directory" },
		"skipped objects pin": func(m *TaskWorktreeProtectedManifest) {
			m.Mounts = slices.Delete(m.Mounts, 7, 8)
			m.Receipts = slices.Delete(m.Receipts, 7, 8)
		},
		"swapped pin order": func(m *TaskWorktreeProtectedManifest) {
			m.Mounts[7], m.Mounts[8] = m.Mounts[8], m.Mounts[7]
			m.Receipts[7], m.Receipts[8] = m.Receipts[8], m.Receipts[7]
		},
		"request source alias": func(m *TaskWorktreeProtectedManifest) { m.Mounts[9].Source = m.Mounts[8].Source },
		"wrong qualifier pid":  func(m *TaskWorktreeProtectedManifest) { m.Receipts[9].QualifierPID++ },
		"stdio descriptor": func(m *TaskWorktreeProtectedManifest) {
			m.Receipts[9].HeldFD = 2
			m.Mounts[9].Source = "/proc/700/fd/2"
		},
		"noncanonical magic source": func(m *TaskWorktreeProtectedManifest) { m.Mounts[9].Source = "/proc/0700/fd/19" },
		"descriptor object alias": func(m *TaskWorktreeProtectedManifest) {
			m.Receipts[9].MountID, m.Receipts[9].Device, m.Receipts[9].Inode = m.Receipts[8].MountID, m.Receipts[8].Device, m.Receipts[8].Inode
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := validManifest(t)
			mutate(&m)
			sealManifest(t, &m)
			q := validQualification(m)
			requireUnsafe(t, VerifyTaskWorktreeDockerInspect(marshalInspect(t, validInspect(m, q)), m, q))
		})
	}
}

func TestEffectiveEnvironmentEvidenceIsClosedAndAuthorityBound(t *testing.T) {
	m := validManifest(t)
	q := validQualification(m)
	identity := validRuntimeIdentity(m, q)
	if err := verifyTaskWorktreeEffectiveEnvironments(m, identity, q); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*TaskWorktreeRuntimeIdentity){
		"missing":   func(i *TaskWorktreeRuntimeIdentity) { i.Environments[0].Entries = i.Environments[0].Entries[1:] },
		"duplicate": func(i *TaskWorktreeRuntimeIdentity) { i.Environments[0].Entries[1] = i.Environments[0].Entries[0] },
		"extra": func(i *TaskWorktreeRuntimeIdentity) {
			i.Environments[0].Entries = append(i.Environments[0].Entries, "ZZZ=x")
		},
		"wrong hostname": func(i *TaskWorktreeRuntimeIdentity) {
			i.Environments[0].Entries[1] = "HOSTNAME=ndq-" + strings.Repeat("9", 32)
		},
		"wrong order": func(i *TaskWorktreeRuntimeIdentity) {
			i.Environments[0].Entries[0], i.Environments[0].Entries[1] = i.Environments[0].Entries[1], i.Environments[0].Entries[0]
		},
		"exec drift":    func(i *TaskWorktreeRuntimeIdentity) { i.Environments[1].Entries[0] = "HOME=/tmp" },
		"incomplete":    func(i *TaskWorktreeRuntimeIdentity) { i.Environments[0].Complete = false },
		"qualification": func(i *TaskWorktreeRuntimeIdentity) { i.Environments[1].QualificationID = "other" },
		"observer":      func(i *TaskWorktreeRuntimeIdentity) { i.Environments[1].ObserverID = "caller" },
		"process":       func(i *TaskWorktreeRuntimeIdentity) { i.Environments[1].ProcessPID = i.InitPID },
		"exec id":       func(i *TaskWorktreeRuntimeIdentity) { i.Environments[1].ExecID = "other" },
		"exec start":    func(i *TaskWorktreeRuntimeIdentity) { i.Environments[1].ProcessStartTime++ },
		"exec digest":   func(i *TaskWorktreeRuntimeIdentity) { i.Environments[1].ExecutableSHA256 = strings.Repeat("9", 64) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			changed := cloneIdentity(identity)
			mutate(&changed)
			requireUnsafe(t, verifyTaskWorktreeEffectiveEnvironments(m, changed, q))
		})
	}
	staleManifest := m
	staleManifest.RunID = strings.Repeat("9", 32)
	staleManifest.Hostname = "ndq-" + staleManifest.RunID
	staleIdentity := cloneIdentity(identity)
	for n := range staleIdentity.Environments {
		for entry := range staleIdentity.Environments[n].Entries {
			if strings.HasPrefix(staleIdentity.Environments[n].Entries[entry], "HOSTNAME=") {
				staleIdentity.Environments[n].Entries[entry] = "HOSTNAME=" + staleManifest.Hostname
			}
		}
	}
	requireUnsafe(t, verifyTaskWorktreeEffectiveEnvironments(staleManifest, staleIdentity, q))
	wrongQualification := q
	wrongQualification.RequestCoreDigest = strings.Repeat("9", 64)
	wrongManifest := m
	bindQualification(t, &wrongManifest, &wrongQualification)
	requireUnsafe(t, verifyTaskWorktreeEffectiveEnvironments(wrongManifest, identity, wrongQualification))
}

func TestInspectSourceReportingModeIsClosed(t *testing.T) {
	for _, mode := range []string{TaskWorktreeSourceMagic, TaskWorktreeSourceCanonical} {
		t.Run(mode, func(t *testing.T) {
			m := validManifest(t)
			q := qualificationFor(m, mode)
			bindQualification(t, &m, &q)
			i := validInspect(m, q)
			if err := VerifyTaskWorktreeDockerInspect(marshalInspect(t, i), m, q); err != nil {
				t.Fatal(err)
			}
			if err := VerifyTaskWorktreeRuntimeState(mountInfoFromBaseline(m.RuntimeBaseline), validStatus(), m, q, validRuntimeIdentity(m, q)); err != nil {
				t.Fatal(err)
			}
			wrongSource := m.Mounts[0].Source
			if mode == TaskWorktreeSourceMagic {
				wrongSource = "/srv/task/00"
			}
			i["Mounts"].([]map[string]any)[0]["Source"] = wrongSource
			requireUnsafe(t, VerifyTaskWorktreeDockerInspect(marshalInspect(t, i), m, q))
		})
	}
	m := validManifest(t)
	for _, mode := range []string{"", "either", "host"} {
		q := validQualification(m)
		q.MountSourceMode = mode
		bindQualification(t, &m, &q)
		requireUnsafe(t, VerifyTaskWorktreeDockerInspect(marshalInspect(t, validInspect(m, q)), m, q))
	}
	m = validManifest(t)
	q := qualificationFor(m, TaskWorktreeSourceCanonical)
	bindQualification(t, &m, &q)
	i := validInspect(m, q)
	q.MountSourceMode = TaskWorktreeSourceMagic
	q.ReportedSources[0].Source = m.Mounts[0].Source
	bindQualification(t, &m, &q)
	requireUnsafe(t, VerifyTaskWorktreeDockerInspect(marshalInspect(t, i), m, q))
	m = validManifest(t)
	q = qualificationFor(m, TaskWorktreeSourceCanonical)
	q.ReportedSources[1].Source = q.ReportedSources[0].Source
	bindQualification(t, &m, &q)
	requireUnsafe(t, VerifyTaskWorktreeDockerInspect(marshalInspect(t, validInspect(m, q)), m, q))
	for name, mutate := range map[string]func(*TaskWorktreeHostQualification){
		"missing":           func(q *TaskWorktreeHostQualification) { q.ReportedSources = q.ReportedSources[1:] },
		"receipt":           func(q *TaskWorktreeHostQualification) { q.ReportedSources[0].ReceiptID = "other" },
		"relative":          func(q *TaskWorktreeHostQualification) { q.ReportedSources[0].Source = "relative" },
		"dot path":          func(q *TaskWorktreeHostQualification) { q.ReportedSources[0].Source = "/srv/task/../other" },
		"trailing slash":    func(q *TaskWorktreeHostQualification) { q.ReportedSources[0].Source += "/" },
		"magic alias":       func(q *TaskWorktreeHostQualification) { q.ReportedSources[0].Source = m.Mounts[1].Source },
		"other magic":       func(q *TaskWorktreeHostQualification) { q.ReportedSources[0].Source = "/proc/701/fd/99" },
		"self magic":        func(q *TaskWorktreeHostQualification) { q.ReportedSources[0].Source = "/proc/self/fd/19" },
		"thread-self magic": func(q *TaskWorktreeHostQualification) { q.ReportedSources[0].Source = "/proc/thread-self/fd/19" },
		"task magic":        func(q *TaskWorktreeHostQualification) { q.ReportedSources[0].Source = "/proc/701/task/702/fd/19" },
	} {
		t.Run("canonical/"+name, func(t *testing.T) {
			m := validManifest(t)
			q := qualificationFor(m, TaskWorktreeSourceCanonical)
			mutate(&q)
			bindQualification(t, &m, &q)
			requireUnsafe(t, VerifyTaskWorktreeDockerInspect(marshalInspect(t, validInspect(m, q)), m, q))
		})
	}
}

func TestQualifiedAbsentRuntimeCoverRequiresHostAndContainerEvidence(t *testing.T) {
	m := manifestWithAbsentPath(t, "/proc/asound")
	q := validQualification(m)
	identity := validRuntimeIdentity(m, q)
	if err := VerifyTaskWorktreeRuntimeState(mountInfoFromBaseline(m.RuntimeBaseline), validStatus(), m, q, identity); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*TaskWorktreeHostQualification, *TaskWorktreeRuntimeIdentity){
		"host missing": func(q *TaskWorktreeHostQualification, _ *TaskWorktreeRuntimeIdentity) { q.QualifiedAbsent = nil },
		"host path": func(q *TaskWorktreeHostQualification, _ *TaskWorktreeRuntimeIdentity) {
			q.QualifiedAbsent[0].Path = "/proc/acpi"
		},
		"host observer": func(q *TaskWorktreeHostQualification, _ *TaskWorktreeRuntimeIdentity) {
			q.QualifiedAbsent[0].ObserverID = "other"
		},
		"host state": func(q *TaskWorktreeHostQualification, _ *TaskWorktreeRuntimeIdentity) {
			q.QualifiedAbsent[0].LstatState = "present"
		},
		"host candidate": func(q *TaskWorktreeHostQualification, _ *TaskWorktreeRuntimeIdentity) {
			q.QualifiedAbsent[0].CandidateSetDigest = ""
		},
		"host namespace": func(q *TaskWorktreeHostQualification, _ *TaskWorktreeRuntimeIdentity) {
			q.QualifiedAbsent[0].HostMountNamespace = ""
		},
		"host root receipt": func(q *TaskWorktreeHostQualification, _ *TaskWorktreeRuntimeIdentity) {
			q.QualifiedAbsent[0].HostRootReceiptID = ""
		},
		"host errno": func(q *TaskWorktreeHostQualification, _ *TaskWorktreeRuntimeIdentity) {
			q.QualifiedAbsent[0].Errno = "EACCES"
		},
		"container missing": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity) { i.QualifiedAbsent = nil },
		"container path": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity) {
			i.QualifiedAbsent[0].Path = "/proc/acpi"
		},
		"container qualification": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity) {
			i.QualifiedAbsent[0].QualificationID = "other"
		},
		"container observer": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity) {
			i.QualifiedAbsent[0].ObserverID = "other"
		},
		"container id": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity) {
			i.QualifiedAbsent[0].ContainerID = strings.Repeat("9", 64)
		},
		"container pid": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity) { i.QualifiedAbsent[0].InitPID++ },
		"container root receipt": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity) {
			i.QualifiedAbsent[0].ProcRootReceiptID = "other"
		},
		"container state": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity) {
			i.QualifiedAbsent[0].LstatState = "present"
		},
		"container errno": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity) {
			i.QualifiedAbsent[0].Errno = "EACCES"
		},
		"paired present": func(q *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity) {
			q.QualifiedAbsent[0].LstatState, i.QualifiedAbsent[0].LstatState = "present", "present"
		},
		"paired access denied": func(q *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity) {
			q.QualifiedAbsent[0].Errno, i.QualifiedAbsent[0].Errno = "EACCES", "EACCES"
		},
	} {
		t.Run(name, func(t *testing.T) {
			cm := m
			cq, ci := q, cloneIdentity(identity)
			cq.QualifiedAbsent = slices.Clone(q.QualifiedAbsent)
			mutate(&cq, &ci)
			bindQualification(t, &cm, &cq)
			requireUnsafe(t, VerifyTaskWorktreeRuntimeState(mountInfoFromBaseline(cm.RuntimeBaseline), validStatus(), cm, cq, ci))
		})
	}
	badManifest := m
	badQualification := validQualification(badManifest)
	badQualification.QualifiedAbsent[0].CandidateSetDigest = strings.Repeat("9", 64)
	bindQualification(t, &badManifest, &badQualification)
	requireUnsafe(t, VerifyTaskWorktreeRuntimeState(mountInfoFromBaseline(badManifest.RuntimeBaseline), validStatus(), badManifest, badQualification, validRuntimeIdentity(badManifest, badQualification)))
}

func TestEveryCanonicalCoverCanUseQualifiedAbsence(t *testing.T) {
	paths := append(CanonicalTaskWorktreeMaskedPaths(), CanonicalTaskWorktreeReadonlyPaths()...)
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			m := manifestWithAbsentPath(t, path)
			q := validQualification(m)
			if err := VerifyTaskWorktreeRuntimeState(mountInfoFromBaseline(m.RuntimeBaseline), validStatus(), m, q, validRuntimeIdentity(m, q)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestQualifiedAbsentEvidenceCannotMixProvenanceOrOrder(t *testing.T) {
	m := validManifest(t)
	paths := []string{"/proc/asound", "/proc/sched_debug"}
	for _, path := range paths {
		for n := range m.RuntimeBaseline.Mounts {
			if m.RuntimeBaseline.Mounts[n].Destination == path {
				m.RuntimeBaseline.Mounts = slices.Delete(m.RuntimeBaseline.Mounts, n, n+1)
				break
			}
		}
	}
	m.RuntimeBaseline.QualifiedAbsent = paths
	q := bindManifestQualification(t, &m, TaskWorktreeSourceMagic)
	identity := validRuntimeIdentity(m, q)
	if err := VerifyTaskWorktreeRuntimeState(mountInfoFromBaseline(m.RuntimeBaseline), validStatus(), m, q, identity); err != nil {
		t.Fatal(err)
	}
	mixed := q
	mixed.QualifiedAbsent = slices.Clone(q.QualifiedAbsent)
	mixed.QualifiedAbsent[1].CandidateSetDigest = strings.Repeat("6", 64)
	sealQualification(t, &mixed)
	requireUnsafe(t, VerifyTaskWorktreeRuntimeState(mountInfoFromBaseline(m.RuntimeBaseline), validStatus(), m, mixed, identity))
	identity.QualifiedAbsent[0], identity.QualifiedAbsent[1] = identity.QualifiedAbsent[1], identity.QualifiedAbsent[0]
	requireUnsafe(t, VerifyTaskWorktreeRuntimeState(mountInfoFromBaseline(m.RuntimeBaseline), validStatus(), m, q, identity))
}

func manifestWithAbsentPath(t *testing.T, path string) TaskWorktreeProtectedManifest {
	t.Helper()
	m := validManifest(t)
	for n := range m.RuntimeBaseline.Mounts {
		if m.RuntimeBaseline.Mounts[n].Destination == path {
			m.RuntimeBaseline.Mounts = slices.Delete(m.RuntimeBaseline.Mounts, n, n+1)
			m.RuntimeBaseline.QualifiedAbsent = []string{path}
			bindManifestQualification(t, &m, TaskWorktreeSourceMagic)
			return m
		}
	}
	t.Fatalf("runtime cover %s is absent", path)
	return TaskWorktreeProtectedManifest{}
}

func TestQualifiedAbsentRejectsOverlapUnknownCoreAndUnqualifiedOmission(t *testing.T) {
	for name, mutate := range map[string]func(*TaskWorktreeProtectedManifest){
		"overlap": func(m *TaskWorktreeProtectedManifest) { m.RuntimeBaseline.QualifiedAbsent = []string{"/proc/asound"} },
		"unknown": func(m *TaskWorktreeProtectedManifest) { m.RuntimeBaseline.QualifiedAbsent = []string{"/other"} },
		"core":    func(m *TaskWorktreeProtectedManifest) { m.RuntimeBaseline.QualifiedAbsent = []string{"/proc"} },
		"unqualified omission": func(m *TaskWorktreeProtectedManifest) {
			for n := range m.RuntimeBaseline.Mounts {
				if m.RuntimeBaseline.Mounts[n].Destination == "/proc/asound" {
					m.RuntimeBaseline.Mounts = slices.Delete(m.RuntimeBaseline.Mounts, n, n+1)
					return
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := validManifest(t)
			mutate(&m)
			sealManifest(t, &m)
			q := validQualification(m)
			requireUnsafe(t, VerifyTaskWorktreeRuntimeState(mountInfoFromBaseline(m.RuntimeBaseline), validStatus(), m, q, validRuntimeIdentity(m, q)))
		})
	}
}

func TestCoreEngineMountCannotSelfAuthenticateAsQualifiedAbsent(t *testing.T) {
	m := validManifest(t)
	for n := range m.RuntimeBaseline.Mounts {
		if m.RuntimeBaseline.Mounts[n].Destination == "/proc" {
			m.RuntimeBaseline.Mounts = slices.Delete(m.RuntimeBaseline.Mounts, n, n+1)
			break
		}
	}
	m.RuntimeBaseline.QualifiedAbsent = []string{"/proc"}
	q := bindManifestQualification(t, &m, TaskWorktreeSourceMagic)
	identity := validRuntimeIdentity(m, q)
	requireUnsafe(t, VerifyTaskWorktreeRuntimeState(mountInfoFromBaseline(m.RuntimeBaseline), validStatus(), m, q, identity))
}

func TestRuntimeEvidenceBindsQualificationSourcesEndpointsAndIdentity(t *testing.T) {
	m := validManifest(t)
	q := validQualification(m)
	identity := validRuntimeIdentity(m, q)
	mountInfo := mountInfoFromBaseline(m.RuntimeBaseline)
	status := validStatus()
	if err := VerifyTaskWorktreeRuntimeState(mountInfo, status, m, q, identity); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*TaskWorktreeHostQualification, *TaskWorktreeRuntimeIdentity, *[]byte){
		"no remap": func(q *TaskWorktreeHostQualification, _ *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			q.UsernsRemapIdentity = ""
		},
		"docker info": func(q *TaskWorktreeHostQualification, _ *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			q.DockerInfoSHA256 = ""
		},
		"foreign container": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.ContainerID = strings.Repeat("9", 64)
		},
		"inspect pid": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.DockerInspectPID++
		},
		"wrong latch": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.InitSHA256 = strings.Repeat("9", 64)
		},
		"identity map": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.UIDMap = []TaskWorktreeIDMap{{Inside: 0, Outside: 0, Length: 65536}}
		},
		"identity map overflow": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.UIDMap = []TaskWorktreeIDMap{{Inside: 0, Outside: ^uint64(0) - 1, Length: 65536}}
		},
		"source": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.Sources[0].Inode++
		},
		"ownership": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.Sources[0].OwnerUID++
		},
		"write access": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.Sources[0].Writable = false
		},
		"endpoint": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.Endpoints[0].ConnectResult = "CONNECTED"
		},
		"endpoint provenance": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.Endpoints[0].ObserverID = "caller"
		},
		"endpoint lstat": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.Endpoints[0].LstatState = "present"
			i.Endpoints[0].FileType = "socket"
		},
		"missing endpoint": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.Endpoints = i.Endpoints[1:]
		},
		"fd": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.InheritedFDs[0].Kind = "socket"
		},
		"fd target": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.InheritedFDs[0].Target = "/dev/null"
		},
		"different pty": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.InheritedFDs[1].Target = "/dev/pts/8"
			i.InheritedFDs[1].MajorMinor = "136:8"
		},
		"malformed pty minor": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.InheritedFDs[0].MajorMinor = "136:"
		},
		"console receipt": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.InheritedFDs[0].ReceiptID = "caller"
		},
		"process tree": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.Processes = i.Processes[:1]
		},
		"latch state": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.Processes[1].State = "S"
		},
		"latch cmdline": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.Processes[1].Cmdline = []string{TaskWorktreeLatchPath, "/usr/local/bin/other"}
		},
		"provider executable": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.ProviderExecutable.SHA256 = strings.Repeat("9", 64)
		},
		"proc root receipt": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.ProcRootReceiptID = ""
		},
		"collector exec id": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.CollectorExecID = ""
		},
		"malformed collector exec id": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.CollectorExecID = "exec-1"
			i.Environments[1].ExecID = i.CollectorExecID
		},
		"collector aliases latch": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.CollectorExecPID = i.Processes[1].PID
			i.Environments[1].ProcessPID = i.CollectorExecPID
		},
		"collector exec pid": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.CollectorExecPID = i.InitPID
		},
		"collector exec start": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.CollectorExecStartTime = 0
		},
		"collector executable": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.CollectorSHA256 = ""
		},
		"filesystem socket": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.OpenUnixSockets.Filesystem = []string{"/run/docker.sock"}
		},
		"abstract socket": func(_ *TaskWorktreeHostQualification, i *TaskWorktreeRuntimeIdentity, _ *[]byte) {
			i.OpenUnixSockets.Abstract = []string{"@docker"}
		},
		"capability": func(_ *TaskWorktreeHostQualification, _ *TaskWorktreeRuntimeIdentity, s *[]byte) {
			*s = []byte(strings.Replace(string(*s), "CapEff:\t0000", "CapEff:\t0001", 1))
		},
		"process status": func(_ *TaskWorktreeHostQualification, _ *TaskWorktreeRuntimeIdentity, s *[]byte) {
			*s = []byte(strings.Replace(string(*s), "State:\tT (stopped)", "State:\tS (sleeping)", 1))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cm := m
			cq := q
			ci := cloneIdentity(identity)
			cs := slices.Clone(status)
			mutate(&cq, &ci, &cs)
			bindQualification(t, &cm, &cq)
			requireUnsafe(t, VerifyTaskWorktreeRuntimeState(mountInfo, cs, cm, cq, ci))
		})
	}
	tamperedQualification := q
	tamperedQualification.Digest = strings.Repeat("9", 64)
	requireUnsafe(t, VerifyTaskWorktreeRuntimeState(mountInfo, status, m, tamperedQualification, identity))
	pairedManifest := m
	pairedQualification := q
	pairedIdentity := cloneIdentity(identity)
	pairedIdentity.CollectorSHA256 = strings.Repeat("9", 64)
	pairedIdentity.Environments[1].ExecutableSHA256 = pairedIdentity.CollectorSHA256
	requireUnsafe(t, VerifyTaskWorktreeRuntimeState(mountInfo, status, m, q, pairedIdentity))
	pairedIdentity = cloneIdentity(identity)
	pairedQualification.CollectorSHA256 = strings.Repeat("9", 64)
	pairedIdentity.CollectorSHA256 = pairedQualification.CollectorSHA256
	pairedIdentity.Environments[1].ExecutableSHA256 = pairedQualification.CollectorSHA256
	sealQualification(t, &pairedQualification)
	requireUnsafe(t, VerifyTaskWorktreeRuntimeState(mountInfo, status, pairedManifest, pairedQualification, pairedIdentity))
}

func TestWritableSourceOwnerCannotSelfAuthenticateAgainstUserNamespace(t *testing.T) {
	m := validManifest(t)
	for n := range m.Receipts {
		if !m.Mounts[n].ReadOnly {
			m.Receipts[n].OwnerUID++
		}
	}
	sealManifest(t, &m)
	q := validQualification(m)
	identity := validRuntimeIdentity(m, q)
	for n := range identity.Sources {
		if !m.Mounts[n].ReadOnly {
			identity.Sources[n].OwnerUID = m.Receipts[n].OwnerUID
		}
	}
	requireUnsafe(t, VerifyTaskWorktreeRuntimeState(mountInfoFromBaseline(m.RuntimeBaseline), validStatus(), m, q, identity))
}

func TestDescriptorObjectAliasFailsWhenManifestAndRuntimeAgree(t *testing.T) {
	m := validManifest(t)
	m.Receipts[1].MountID = m.Receipts[0].MountID
	m.Receipts[1].Device = m.Receipts[0].Device
	m.Receipts[1].Inode = m.Receipts[0].Inode
	for n := range m.RuntimeBaseline.Mounts {
		if m.RuntimeBaseline.Mounts[n].ReceiptID == m.Receipts[1].ReceiptID {
			m.RuntimeBaseline.Mounts[n].ReceiptMountID = m.Receipts[1].MountID
			m.RuntimeBaseline.Mounts[n].ReceiptDevice = m.Receipts[1].Device
		}
	}
	sealManifest(t, &m)
	q := validQualification(m)
	identity := validRuntimeIdentity(m, q)
	identity.Sources[1].MountID = identity.Sources[0].MountID
	identity.Sources[1].Device = identity.Sources[0].Device
	identity.Sources[1].Inode = identity.Sources[0].Inode
	requireUnsafe(t, VerifyTaskWorktreeRuntimeState(mountInfoFromBaseline(m.RuntimeBaseline), validStatus(), m, q, identity))
}

func TestMountInfoDecoderIsOnePass(t *testing.T) {
	got, err := parseTaskMountInfo([]byte("1 0 0:1 /a\\134040 /c\\040d ro - tmpfs tmpfs ro\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Root != `/a\040` || got[0].Destination != "/c d" {
		t.Fatalf("double-decoded mountinfo: %#v", got[0])
	}
}

func TestNamespaceAndGitBinding(t *testing.T) {
	state := TaskWorktreeNamespaceState{Host: map[string]string{}, Container: map[string]string{}}
	for n, name := range []string{"mnt", "pid", "ipc", "net", "user", "cgroup"} {
		state.Host[name] = fmt.Sprint(n)
		state.Container[name] = fmt.Sprint(n + 10)
	}
	if err := VerifyTaskWorktreeNamespaceState(state); err != nil {
		t.Fatal(err)
	}
	bad := TaskWorktreeNamespaceState{Host: maps.Clone(state.Host), Container: maps.Clone(state.Container)}
	bad.Container["user"] = bad.Host["user"]
	requireUnsafe(t, VerifyTaskWorktreeNamespaceState(bad))
	valid := TaskWorktreeGitBindingState{"gitdir: /project.git/worktrees/assigned\n", "/project/.git\n", "../..\n", "false"}
	if err := VerifyTaskWorktreeGitBinding(valid); err != nil {
		t.Fatal(err)
	}
	valid.WorktreeConfig = "true"
	requireUnsafe(t, VerifyTaskWorktreeGitBinding(valid))
}

func validManifest(t *testing.T) TaskWorktreeProtectedManifest {
	t.Helper()
	m := TaskWorktreeProtectedManifest{
		Schema: TaskWorktreeManifestSchemaV2, Profile: TaskWorktreeContainerProfile, AuthorityRevision: "policy-7",
		ProviderProfileID: "codex-v1", ProviderProfileRevision: "codex-v1@0.148.0",
		RunID: strings.Repeat("1", 32), Hostname: "ndq-" + strings.Repeat("1", 32), QualifierBuildDigest: strings.Repeat("2", 64), RequestCoreDigest: strings.Repeat("3", 64),
		ImageID: "sha256:" + strings.Repeat("a", 64), ContainerID: strings.Repeat("b", 64), TiniSHA256: strings.Repeat("d", 64), LatchSHA256: strings.Repeat("c", 64),
		User: "1026:100", Runtime: TaskWorktreeRuntime, Entrypoint: []string{"/usr/bin/tini", "-g", "--", TaskWorktreeLatchPath}, ProviderArgv: []string{"/usr/local/bin/provider", "--safe"},
		ProviderExecutable: TaskWorktreeExecutableReceipt{"/usr/local/bin/provider", strings.Repeat("e", 64), "regular", 99, 8, 1000}, Console: TaskWorktreeConsoleReceipt{"console-1", "character", "/dev/pts/7", "136:7", 136, 700},
		ConfigEnv: CanonicalTaskWorktreeConfigEnv(), WorkingDir: "/project", NetworkMode: "deck-task-network", NetworkID: strings.Repeat("f", 64), DockerDataRoot: "/var/lib/docker",
		SecurityOpt: []string{"no-new-privileges:true"}, Tmpfs: CanonicalTaskWorktreeTmpfs(), MaskedPaths: CanonicalTaskWorktreeMaskedPaths(), ReadonlyPaths: CanonicalTaskWorktreeReadonlyPaths(),
	}
	for n, fixed := range goldenMountRoles() {
		receiptID := fmt.Sprintf("receipt-%02d", n)
		m.Mounts = append(m.Mounts, TaskWorktreeMountExpectation{Role: fixed.Role, Type: "bind", Source: fmt.Sprintf("/proc/700/fd/%d", n+10), ReceiptID: receiptID, Destination: fixed.Destination, Propagation: "rprivate", ReadOnly: fixed.ReadOnly})
		m.Receipts = append(m.Receipts, TaskWorktreeSourceReceipt{
			Schema: TaskWorktreeReceiptSchemaV2, ReceiptID: receiptID, ProtectedBackingID: "backing-1", TasksReceiptID: "tasks-receipt-1",
			Role: fixed.Role, RelativeRole: fixed.RelativeRole, Kind: fixed.SourceKind,
			MountID: uint64(n + 1), Device: uint64(n + 1), Inode: uint64(n + 101), HeldFDGeneration: 1,
			OwnerUID: 101026, OwnerGID: 100100, QualifierPID: 700, HeldFD: uint32(n + 10),
		})
	}
	m.RuntimeBaseline = independentBaseline(m.Receipts, m.ContainerID, m.DockerDataRoot)
	bindManifestQualification(t, &m, TaskWorktreeSourceMagic)
	return m
}

func goldenMountRoles() []TaskWorktreeMountRole {
	return []TaskWorktreeMountRole{
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
}

func independentBaseline(receipts []TaskWorktreeSourceReceipt, containerID, dockerDataRoot string) TaskWorktreeRuntimeBaseline {
	b := TaskWorktreeRuntimeBaseline{Schema: TaskWorktreeBaselineSchemaV2}
	for n, fixed := range goldenMountRoles() {
		mode := "rw"
		if fixed.ReadOnly {
			mode = "ro"
		}
		b.Mounts = append(b.Mounts, TaskWorktreeRuntimeMount{Class: "topology", SourceClass: "project-backing", ReceiptID: receipts[n].ReceiptID, ReceiptMountID: receipts[n].MountID, ReceiptDevice: receipts[n].Device, Root: fmt.Sprintf("/source/%d", n), Destination: fixed.Destination, MajorMinor: fmt.Sprintf("%d:%d", receipts[n].Device>>32, uint32(receipts[n].Device)), MountOptions: []string{mode, "nosuid", "nodev"}, FSType: "ext4", Source: "/dev/root", SuperOptions: []string{mode}})
	}
	for _, destination := range []string{"/home/agent", "/run/noisedeck", "/tmp"} {
		options := []string{"rw", "nosuid", "nodev"}
		if destination != "/tmp" {
			options = append(options, "noexec")
		}
		b.Mounts = append(b.Mounts, TaskWorktreeRuntimeMount{Class: "scratch", SourceClass: "scratch-tmpfs", Root: "/", Destination: destination, MajorMinor: "0:1", MountOptions: options, FSType: "tmpfs", Source: "tmpfs", SuperOptions: []string{"rw"}})
	}
	engine := []TaskWorktreeRuntimeMount{
		{Root: "/", Destination: "/", MountOptions: []string{"ro"}, FSType: "overlay", Source: "overlay", SuperOptions: []string{"rw"}},
		{Root: "/", Destination: "/proc", MountOptions: []string{"rw", "nosuid", "nodev", "noexec"}, FSType: "proc", Source: "proc", SuperOptions: []string{"rw"}},
		{Root: "/", Destination: "/dev", MountOptions: []string{"rw", "nosuid"}, FSType: "tmpfs", Source: "tmpfs", SuperOptions: []string{"rw"}},
		{Root: "/", Destination: "/dev/pts", MountOptions: []string{"rw", "nosuid", "noexec"}, FSType: "devpts", Source: "devpts", SuperOptions: []string{"rw"}},
		{Root: "/", Destination: "/dev/mqueue", MountOptions: []string{"rw", "nosuid", "nodev", "noexec"}, FSType: "mqueue", Source: "mqueue", SuperOptions: []string{"rw"}},
		{Root: "/", Destination: "/dev/shm", MountOptions: []string{"rw", "nosuid", "nodev", "noexec"}, FSType: "tmpfs", Source: "shm", SuperOptions: []string{"rw"}},
		{Root: "/", Destination: "/sys", MountOptions: []string{"ro", "nosuid", "nodev", "noexec"}, FSType: "sysfs", Source: "sysfs", SuperOptions: []string{"ro"}},
		{Root: "/", Destination: "/sys/fs/cgroup", MountOptions: []string{"ro", "nosuid", "nodev", "noexec"}, FSType: "cgroup2", Source: "cgroup", SuperOptions: []string{"rw"}},
	}
	for _, destination := range []string{"/etc/hosts", "/etc/hostname", "/etc/resolv.conf"} {
		engine = append(engine, TaskWorktreeRuntimeMount{SourceClass: "engine-container-file", Root: filepath.Join(dockerDataRoot, "containers", containerID, filepath.Base(destination)), Destination: destination, MountOptions: []string{"rw"}, FSType: "ext4", Source: "/dev/root", SuperOptions: []string{"rw"}})
	}
	for _, destination := range []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"} {
		engine = append(engine, TaskWorktreeRuntimeMount{Root: strings.TrimPrefix(destination, "/proc"), Destination: destination, MountOptions: []string{"ro", "nosuid", "nodev", "noexec"}, FSType: "proc", Source: "proc", SuperOptions: []string{"rw"}})
	}
	for _, destination := range []string{"/proc/interrupts", "/proc/kcore", "/proc/keys", "/proc/latency_stats", "/proc/sched_debug", "/proc/timer_list", "/proc/timer_stats"} {
		engine = append(engine, TaskWorktreeRuntimeMount{Root: "/null", Destination: destination, MountOptions: []string{"rw", "nosuid"}, FSType: "tmpfs", Source: "tmpfs", SuperOptions: []string{"rw"}})
	}
	for _, destination := range []string{"/proc/acpi", "/proc/asound", "/proc/scsi", "/sys/firmware", "/sys/devices/virtual/powercap"} {
		engine = append(engine, TaskWorktreeRuntimeMount{Root: "/", Destination: destination, MountOptions: []string{"ro", "nosuid", "nodev", "noexec"}, FSType: "tmpfs", Source: "tmpfs", SuperOptions: []string{"ro"}})
	}
	for _, mount := range engine {
		mount.Class, mount.MajorMinor = "engine-pseudo", "0:2"
		if mount.SourceClass == "" {
			mount.SourceClass = "engine"
		}
		b.Mounts = append(b.Mounts, mount)
	}
	d, _ := CanonicalTaskWorktreeBaselineDigest(b)
	b.Digest = d
	return b
}

func sealManifest(t *testing.T, m *TaskWorktreeProtectedManifest) {
	t.Helper()
	d, err := CanonicalTaskWorktreeBaselineDigest(m.RuntimeBaseline)
	if err != nil {
		t.Fatal(err)
	}
	m.RuntimeBaseline.Digest = d
	d, err = CanonicalTaskWorktreeManifestDigest(*m)
	if err != nil {
		t.Fatal(err)
	}
	m.Digest = d
}

func sealQualification(t *testing.T, q *TaskWorktreeHostQualification) {
	t.Helper()
	digest, err := canonicalTaskWorktreeQualificationDigest(*q)
	if err != nil {
		t.Fatal(err)
	}
	q.Digest = digest
}

func bindManifestQualification(t *testing.T, m *TaskWorktreeProtectedManifest, mode string) TaskWorktreeHostQualification {
	t.Helper()
	baselineDigest, err := CanonicalTaskWorktreeBaselineDigest(m.RuntimeBaseline)
	if err != nil {
		t.Fatal(err)
	}
	m.RuntimeBaseline.Digest = baselineDigest
	q := qualificationFor(*m, mode)
	m.HostQualificationDigest = q.Digest
	manifestDigest, err := CanonicalTaskWorktreeManifestDigest(*m)
	if err != nil {
		t.Fatal(err)
	}
	m.Digest = manifestDigest
	return q
}

func bindQualification(t *testing.T, m *TaskWorktreeProtectedManifest, q *TaskWorktreeHostQualification) {
	t.Helper()
	sealQualification(t, q)
	m.HostQualificationDigest = q.Digest
	sealManifest(t, m)
}

func validInspect(m TaskWorktreeProtectedManifest, q TaskWorktreeHostQualification) map[string]any {
	mounts := make([]map[string]any, len(m.Mounts))
	for n, mount := range m.Mounts {
		mode := "rw"
		if mount.ReadOnly {
			mode = "ro"
		}
		mounts[n] = map[string]any{"Type": mount.Type, "Source": expectedInspectSource(q, n), "Destination": mount.Destination, "Mode": mode, "RW": !mount.ReadOnly, "Propagation": mount.Propagation}
	}
	args := append([]string{"-g", "--", TaskWorktreeLatchPath}, m.ProviderArgv...)
	return map[string]any{"Id": m.ContainerID, "Image": m.ImageID, "Path": "/usr/bin/tini", "Args": args, "RestartCount": 0, "State": map[string]any{"Status": "running", "Running": true, "Paused": false, "Restarting": false, "OOMKilled": false, "Dead": false, "Pid": 42, "ExitCode": 0, "Error": "", "StartedAt": "2026-01-01T00:00:00Z", "FinishedAt": "0001-01-01T00:00:00Z"}, "Config": map[string]any{"User": m.User, "Hostname": m.Hostname, "Labels": canonicalTaskWorktreeLabels(m), "Entrypoint": m.Entrypoint, "Cmd": m.ProviderArgv, "Env": m.ConfigEnv, "WorkingDir": m.WorkingDir, "StopSignal": "SIGTERM", "StopTimeout": nil, "AttachStdin": true, "AttachStdout": true, "AttachStderr": true, "OpenStdin": true, "StdinOnce": false, "Tty": true, "ExposedPorts": map[string]any{}, "Healthcheck": map[string]any{"Test": []string{"NONE"}}}, "HostConfig": map[string]any{"ReadonlyRootfs": true, "Privileged": false, "AutoRemove": false, "Init": false, "PublishAllPorts": false, "CapAdd": []string{}, "CapDrop": []string{"ALL"}, "SecurityOpt": m.SecurityOpt, "NetworkMode": m.NetworkMode, "PidMode": "", "IpcMode": "private", "UsernsMode": "", "CgroupnsMode": "private", "Runtime": m.Runtime, "CgroupParent": "", "UTSMode": "", "Isolation": "", "ContainerIDFile": "", "DeviceCgroupRules": []string{}, "Links": []string{}, "RestartPolicy": map[string]any{"Name": "no", "MaximumRetryCount": 0}, "LogConfig": map[string]any{"Type": "none", "Config": map[string]string{}}, "GroupAdd": []string{}, "VolumesFrom": []string{}, "ExtraHosts": []string{}, "Devices": []any{}, "DeviceRequests": []any{}, "Sysctls": map[string]string{}, "Tmpfs": maps.Clone(m.Tmpfs), "MaskedPaths": m.MaskedPaths, "ReadonlyPaths": m.ReadonlyPaths, "PortBindings": map[string]any{}}, "Mounts": mounts, "NetworkSettings": map[string]any{"Networks": map[string]any{m.NetworkMode: map[string]any{"NetworkID": m.NetworkID}}}}
}

func validQualification(m TaskWorktreeProtectedManifest) TaskWorktreeHostQualification {
	return qualificationFor(m, TaskWorktreeSourceMagic)
}

func qualificationFor(m TaskWorktreeProtectedManifest, mode string) TaskWorktreeHostQualification {
	q := TaskWorktreeHostQualification{
		Schema: TaskWorktreeHostQualificationSchemaV2, QualificationID: "host-q-1", DockerEngineID: "docker-1", Runtime: TaskWorktreeRuntime,
		DockerInfoSHA256: strings.Repeat("1", 64), UsernsRemapIdentity: "dockremap:100000:65536", ObserverID: "observer-1",
		ProviderProfileID: m.ProviderProfileID, ProviderProfileRevision: m.ProviderProfileRevision, RequestCoreDigest: m.RequestCoreDigest,
		RuntimeBaselineDigest: m.RuntimeBaseline.Digest, CollectorSHA256: strings.Repeat("5", 64), MountSourceMode: mode,
	}
	for n, mount := range m.Mounts {
		source := mount.Source
		if mode == TaskWorktreeSourceCanonical {
			source = fmt.Sprintf("/srv/task/%02d", n)
		}
		q.ReportedSources = append(q.ReportedSources, TaskWorktreeReportedSource{ReceiptID: mount.ReceiptID, Source: source})
	}
	for _, path := range m.RuntimeBaseline.QualifiedAbsent {
		q.QualifiedAbsent = append(q.QualifiedAbsent, TaskWorktreeAbsentPathQualification{Path: path, ObserverID: q.ObserverID, CandidateSetDigest: taskWorktreeCandidateSetDigest(), HostMountNamespace: "mnt:[100]", HostRootReceiptID: "host-root-1", LstatState: "absent", Errno: "ENOENT"})
	}
	q.Digest, _ = canonicalTaskWorktreeQualificationDigest(q)
	return q
}

func validStatus() []byte {
	return []byte("State:\tT (stopped)\nCapInh:\t0000\nCapPrm:\t0000\nCapEff:\t0000\nCapBnd:\t0000\nCapAmb:\t0000\nNoNewPrivs:\t1\nUid:\t1026\t1026\t1026\t1026\nGid:\t100\t100\t100\t100\n")
}

func validRuntimeIdentity(m TaskWorktreeProtectedManifest, q TaskWorktreeHostQualification) TaskWorktreeRuntimeIdentity {
	tiniCmd := append([]string{"/usr/bin/tini", "-g", "--", TaskWorktreeLatchPath}, m.ProviderArgv...)
	latchCmd := append([]string{TaskWorktreeLatchPath}, m.ProviderArgv...)
	fds := make([]TaskWorktreeInheritedFD, 3)
	for n := range fds {
		fds[n] = TaskWorktreeInheritedFD{Number: n, ReceiptID: m.Console.ReceiptID, Kind: m.Console.Kind, Target: m.Console.Target, MajorMinor: m.Console.MajorMinor, Device: m.Console.Device, Inode: m.Console.Inode}
	}
	effectiveEnvironment := append(CanonicalTaskWorktreeConfigEnv(), "HOSTNAME="+m.Hostname)
	slices.SortFunc(effectiveEnvironment, compareEnvironmentNames)
	i := TaskWorktreeRuntimeIdentity{
		ContainerID: m.ContainerID, InitPID: 42, DockerInspectPID: 42, InitDevice: 1, InitInode: 2, InitSHA256: m.TiniSHA256,
		EffectiveUID: 1026, EffectiveGID: 100, UIDMap: []TaskWorktreeIDMap{{0, 100000, 65536}}, GIDMap: []TaskWorktreeIDMap{{0, 100000, 65536}},
		InheritedFDs: fds, Processes: []TaskWorktreeProcessIdentity{{42, 0, m.TiniSHA256, "init", "S", tiniCmd}, {43, 42, m.LatchSHA256, "latch", "T", latchCmd}},
		ProviderExecutable: m.ProviderExecutable, OpenUnixSockets: TaskWorktreeUnixSocketEvidence{q.ObserverID, m.ContainerID, 42, true, nil, nil},
		ProcRootReceiptID: "proc-root-1", CollectorExecID: strings.Repeat("6", 64), CollectorExecPID: 44, CollectorExecStartTime: 900, CollectorSHA256: q.CollectorSHA256,
	}
	i.Environments = []TaskWorktreeEnvironmentObservation{
		{Role: "initial", QualificationID: q.QualificationID, ObserverID: q.ObserverID, ContainerID: m.ContainerID, InitPID: 42, ProcessPID: 42, Complete: true, Entries: slices.Clone(effectiveEnvironment)},
		{Role: "exec", QualificationID: q.QualificationID, ObserverID: q.ObserverID, ContainerID: m.ContainerID, ExecID: i.CollectorExecID, ExecutableSHA256: i.CollectorSHA256, InitPID: 42, ProcessPID: i.CollectorExecPID, ProcessStartTime: i.CollectorExecStartTime, Complete: true, Entries: slices.Clone(effectiveEnvironment)},
	}
	for n, r := range m.Receipts {
		i.Sources = append(i.Sources, TaskWorktreeObservedSource{r.ReceiptID, expectedInspectSource(q, n), r.Kind, r.MountID, r.Device, r.Inode, r.HeldFDGeneration, r.OwnerUID, r.OwnerGID, true, !m.Mounts[n].ReadOnly})
	}
	for _, path := range taskWorktreeEndpointPaths {
		i.Endpoints = append(i.Endpoints, TaskWorktreeEndpointObservation{q.ObserverID, m.ContainerID, path, "absent", "none", "ENOENT", 42})
	}
	for _, path := range m.RuntimeBaseline.QualifiedAbsent {
		i.QualifiedAbsent = append(i.QualifiedAbsent, TaskWorktreeAbsentPathObservation{Path: path, QualificationID: q.QualificationID, ObserverID: q.ObserverID, ContainerID: m.ContainerID, ProcRootReceiptID: i.ProcRootReceiptID, LstatState: "absent", Errno: "ENOENT", InitPID: 42})
	}
	return i
}
func cloneIdentity(i TaskWorktreeRuntimeIdentity) TaskWorktreeRuntimeIdentity {
	i.UIDMap = slices.Clone(i.UIDMap)
	i.GIDMap = slices.Clone(i.GIDMap)
	i.Sources = slices.Clone(i.Sources)
	i.Endpoints = slices.Clone(i.Endpoints)
	i.InheritedFDs = slices.Clone(i.InheritedFDs)
	i.OpenUnixSockets.Filesystem = slices.Clone(i.OpenUnixSockets.Filesystem)
	i.OpenUnixSockets.Abstract = slices.Clone(i.OpenUnixSockets.Abstract)
	i.Processes = slices.Clone(i.Processes)
	i.QualifiedAbsent = slices.Clone(i.QualifiedAbsent)
	i.Environments = slices.Clone(i.Environments)
	for n := range i.Environments {
		i.Environments[n].Entries = slices.Clone(i.Environments[n].Entries)
	}
	return i
}

func TestTaskWorktreeCanonicalBridgeReturnsDefensiveCopies(t *testing.T) {
	endpoints := CanonicalTaskWorktreeEndpointPaths()
	candidates := CanonicalTaskWorktreeCoverCandidates()
	pseudo := CanonicalTaskWorktreeEnginePseudoDestinations()
	if len(endpoints) != 7 || len(candidates) != 17 || len(pseudo) == 0 || CanonicalTaskWorktreeCandidateSetDigest() != taskWorktreeCandidateSetDigest() {
		t.Fatal("canonical bridge vectors differ")
	}
	endpoints[0], candidates[0], pseudo[0] = "changed", "changed", "changed"
	if CanonicalTaskWorktreeEndpointPaths()[0] == "changed" || CanonicalTaskWorktreeCoverCandidates()[0] == "changed" || CanonicalTaskWorktreeEnginePseudoDestinations()[0] == "changed" {
		t.Fatal("caller mutated a canonical bridge vector")
	}
}
func mountInfoFromBaseline(b TaskWorktreeRuntimeBaseline) []byte {
	var lines []string
	for n, m := range b.Mounts {
		lines = append(lines, fmt.Sprintf("%d 1 %s %s %s %s %s - %s %s %s", n+10, m.MajorMinor, m.Root, m.Destination, strings.Join(m.MountOptions, ","), strings.Join(m.OptionalFields, " "), m.FSType, m.Source, strings.Join(m.SuperOptions, ",")))
	}
	return []byte(strings.ReplaceAll(strings.Join(lines, "\n")+"\n", "  -", " -"))
}
func runtimeMountAt(t *testing.T, baseline *TaskWorktreeRuntimeBaseline, destination string) *TaskWorktreeRuntimeMount {
	t.Helper()
	for n := range baseline.Mounts {
		if baseline.Mounts[n].Destination == destination {
			return &baseline.Mounts[n]
		}
	}
	t.Fatalf("runtime mount %s is absent", destination)
	return nil
}
func marshalInspect(t *testing.T, i map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal([]any{i})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func nested(v map[string]any, key string) map[string]any { return v[key].(map[string]any) }
func requireUnsafe(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrTaskWorktreeEffectiveState) {
		t.Fatalf("got %v", err)
	}
}

var _ = reflect.DeepEqual
