//go:build linux

package workspaceplain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const inspectFormat = `{"id":{{json .Id}},"running":{{.State.Running}},"pid":{{.State.Pid}},"status":{{json .State.Status}},"started":{{json .State.StartedAt}},"health":{{json .State.Healthcheck.Status}},"project":{{json (index .Config.Labels "com.docker.compose.project")}},"podmanProject":{{json (index .Config.Labels "io.podman.compose.project")}},"ports":{{json .NetworkSettings.Ports}},"network":{{json .HostConfig.NetworkMode}},"mounts":{{json .Mounts}},"tmpfs":{{json .HostConfig.Tmpfs}}}`

// Only selected nonsecret transport markers leave the engine. Its environment
// also holds OIDC configuration, which must never be copied into diagnostics.
const transportFormat = `{{$canonical := false}}{{$u := false}}{{range .Config.Env}}{{if eq . "NOISEDECK_SERVER_BIND="}}b{{end}}{{if eq . "NOISEDECK_SERVER_UNIX_SOCKET=/data/gateway/backend.sock"}}s{{end}}{{if eq . "NOISEDECK_PROJECT_PREVIEWS_BIND="}}p{{end}}{{if eq . "NOISEDECK_EXPERIMENTS_UNCONFINED_CAPACITY_ROUTING=false"}}c{{end}}{{if eq . "NOISEDECK_AUTH_MODE=oidc"}}a{{end}}{{if eq (printf "%.41s" .) "NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE="}}{{$canonical = true}}{{end}}{{if eq . "NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE=false"}}{{$u = true}}{{end}}{{end}}{{if $u}}u{{else if not $canonical}}{{range .Config.Env}}{{if eq . "NOISEDECK_REQUIRE_ENCRYPTED_STATE=false"}}u{{end}}{{end}}{{end}}`

var containerID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func bootIdentity() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return "", ErrUnavailable
	}
	return strings.TrimSpace(string(b)), nil
}

func processIdentity(pid, uid int) (string, error) {
	if pid <= 0 {
		return "", ErrUnavailable
	}
	path := "/proc/" + strconv.Itoa(pid)
	var st unix.Stat_t
	if unix.Stat(path, &st) != nil || int(st.Uid) != uid {
		return "", ErrUnavailable
	}
	// #nosec G304 -- PID is an observed integer; the path is confined to procfs.
	b, err := os.ReadFile(path + "/stat")
	if err != nil {
		return "", ErrUnavailable
	}
	_, tail, ok := strings.Cut(string(b), ") ")
	fields := strings.Fields(tail)
	if !ok || len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return "", ErrUnavailable
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return "", ErrUnavailable
	}
	return strconv.FormatUint(start, 10), nil
}

func (c *Controller) observe(ctx context.Context) (State, error) {
	s := State{Runtime: "unknown"}
	configured, err := c.layout(false)
	if err != nil {
		return s, err
	}
	s.Configured = configured
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	boot, err := c.boot()
	if err != nil {
		return s, ErrUnavailable
	}
	rootless, err := c.command(ctx, c.cfg.PodmanPath, []string{"info", "--format", "{{.Host.Security.Rootless}}"}, true)
	if err != nil || strings.TrimSpace(string(rootless)) != "true" {
		return s, ErrUnavailable
	}
	ids := map[string]bool{}
	for _, label := range []string{"com.docker.compose.project", "io.podman.compose.project"} {
		out, e := c.command(ctx, c.cfg.PodmanPath, []string{"ps", "--all", "--no-trunc", "--filter", "label=" + label + "=" + c.cfg.ComposeProject, "--format", "{{.ID}}"}, true)
		if e != nil {
			return s, ErrUnavailable
		}
		for _, id := range strings.Fields(string(out)) {
			if !containerID.MatchString(id) {
				return s, ErrUnavailable
			}
			ids[id] = true
		}
	}
	_, err = c.run.run(ctx, c.cfg.PodmanPath, []string{"container", "exists", c.cfg.AppContainer}, c.environment(), true)
	if exitCode(err) == 1 {
		if len(ids) != 0 {
			return s, ErrUnavailable
		}
		s.Runtime = "stopped"
		s.Epoch = c.epoch(boot, "absent")
		return s, nil
	}
	if err != nil || !configured {
		return s, ErrUnavailable
	}
	out, err := c.command(ctx, c.cfg.PodmanPath, []string{"inspect", "--format", inspectFormat, c.cfg.AppContainer}, true)
	var app struct {
		ID                                                       string
		Running                                                  bool
		PID                                                      int
		Status, Started, Health, Project, PodmanProject, Network string
		Ports                                                    map[string][]json.RawMessage
		Tmpfs                                                    map[string]string
		Mounts                                                   []struct {
			Type, Source, Destination string
			RW                        bool
		}
	}
	if err != nil || json.Unmarshal(out, &app) != nil || !containerID.MatchString(app.ID) || len(ids) != 1 || !ids[app.ID] ||
		(app.Project != c.cfg.ComposeProject && app.PodmanProject != c.cfg.ComposeProject) ||
		(app.Project != "" && app.Project != c.cfg.ComposeProject) || (app.PodmanProject != "" && app.PodmanProject != c.cfg.ComposeProject) {
		return s, ErrUnavailable
	}
	if app.Network == "" || app.Network == "host" || strings.HasPrefix(app.Network, "ns:") || strings.HasPrefix(app.Network, "container:") {
		return s, ErrUnavailable
	}
	for _, ports := range app.Ports {
		if len(ports) != 0 {
			return s, ErrUnavailable
		}
	}
	want := map[string]string{"/data": "data", "/home/agent": "home", "/workspace": "projects", "/tmp": "tmp", "/home/agent/.config/gh": "gh"}
	seen := map[string]bool{}
	for _, mount := range app.Mounts {
		if mount.Type != "bind" || seen[mount.Destination] {
			return s, ErrStorage
		}
		if rel, ok := want[mount.Destination]; ok {
			if !mount.RW || mount.Source != filepath.Join(c.cfg.StateRoot, rel) {
				return s, ErrStorage
			}
		} else if mount.Destination != "/run/noisedeck-host-machine-id" || mount.RW || !filepath.IsAbs(mount.Source) {
			return s, ErrStorage
		}
		seen[mount.Destination] = true
	}
	if len(seen) != len(want)+1 {
		return s, ErrStorage
	}
	// Podman lists user tmpfs separately; an overlay beneath a managed bind
	// would otherwise let workspace writes disappear outside its pinned store.
	if _, ok := app.Tmpfs["/run/noisedeck"]; !ok || len(app.Tmpfs) != 1 {
		return s, ErrStorage
	}
	markers, err := c.command(ctx, c.cfg.PodmanPath, []string{"inspect", "--format", transportFormat, c.cfg.AppContainer}, true)
	m := strings.TrimSpace(string(markers))
	if err != nil || len(m) != 6 {
		return s, ErrUnavailable
	}
	for _, char := range "bspcau" {
		if strings.Count(m, string(char)) != 1 {
			return s, ErrUnavailable
		}
	}
	identity := fmt.Sprintf("%s/%d/%s", app.ID, app.PID, app.Started)
	if !app.Running && app.PID == 0 && (app.Status == "exited" || app.Status == "stopped" || app.Status == "created" || app.Status == "configured") {
		s.Runtime = "stopped"
		s.Epoch = c.epoch(boot, identity)
		return s, nil
	}
	started, e := time.Parse(time.RFC3339Nano, app.Started)
	if !app.Running || app.Status != "running" || app.PID <= 0 || e != nil || started.IsZero() {
		return s, ErrUnavailable
	}
	process, e := c.process(app.PID, c.cfg.OwnerUID)
	if e != nil {
		return s, ErrUnavailable
	}
	if app.Health == "starting" {
		s.Runtime = "starting"
		s.Epoch = c.epoch(boot, identity+"/"+process)
		return s, nil
	}
	if app.Health != "healthy" {
		return s, ErrUnavailable
	}
	// A stale engine health record alone does not establish current readiness.
	if _, err = c.command(ctx, c.cfg.PodmanPath, []string{"exec", c.cfg.AppContainer, "/usr/local/bin/noisedeck-auth-mode-healthcheck"}, false); err != nil {
		return s, ErrUnavailable
	}
	var socket unix.Stat_t
	if unix.Lstat(filepath.Join(c.cfg.StateRoot, "data/gateway/backend.sock"), &socket) != nil || socket.Mode&unix.S_IFMT != unix.S_IFSOCK || int(socket.Uid) != c.cfg.OwnerUID || socket.Mode&0077 != 0 {
		return s, ErrUnavailable
	}
	if again, e := c.process(app.PID, c.cfg.OwnerUID); e != nil || again != process {
		return s, ErrUnavailable
	}
	if _, err = c.layout(true); err != nil {
		return s, err
	}
	s.Runtime = "running"
	s.Epoch = c.epoch(boot, identity+"/"+process)
	return s, nil
}

func (c *Controller) epoch(boot, runtime string) string {
	raw, _ := json.Marshal(c.rootIdentity)
	sum := sha256.Sum256([]byte(c.fingerprint + "\x00" + string(raw) + "\x00" + boot + "\x00" + runtime))
	return hex.EncodeToString(sum[:])
}
