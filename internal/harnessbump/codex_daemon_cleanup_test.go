package harnessbump

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCodexProbeStopsOnlyItsSyntheticDaemon(t *testing.T) {
	source, err := filepath.Abs(filepath.Join("..", "..", "deploy", "mempalace", "probe_a2a_codex.py"))
	if err != nil {
		t.Fatal(err)
	}
	const script = `
import importlib.util, os, pathlib, subprocess, sys, tempfile, types
from unittest import mock
source = pathlib.Path(sys.argv[1])
sys.path.insert(0, str(source.parent))
spec = importlib.util.spec_from_file_location("codex_probe", source)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
with tempfile.TemporaryDirectory() as root:
    root = pathlib.Path(root)
    own, foreign, bin = root / "own", root / "foreign", root / "bin"
    for p in (own, foreign, bin): p.mkdir()
    children = [subprocess.Popen(["sleep", "60"]) for _ in range(2)]
    try:
        (own / "daemon.pid").write_text(str(children[0].pid))
        (foreign / "daemon.pid").write_text(str(children[1].pid))
        cli = bin / "codex"
        cli.write_text("#!" + sys.executable + "\n" + """import os, pathlib, signal, sys
assert sys.argv[1:] == ["app-server", "daemon", "stop"]
assert "CODEX_HOME" not in os.environ
assert os.environ["PROBE_API_KEY"] == "probe-dummy-key"
home = pathlib.Path(os.environ["HOME"])
if (home / "refuse").exists():
    print("native stop denied", file=sys.stderr)
    raise SystemExit(42)
try: os.kill(int((home / "daemon.pid").read_text()), signal.SIGTERM)
except ProcessLookupError: pass
""")
        cli.chmod(0o755)
        os.environ.update(HOME=str(foreign), CODEX_HOME=str(foreign / ".codex"))
        def scoped_env(extra):
            return {"HOME": str(own), "PATH": str(bin) + ":/usr/bin:/bin", **extra}
        def fail(message): raise RuntimeError(message)
        probe = types.SimpleNamespace(root=own, env=scoped_env, fail=fail)
        module.kill_daemons(probe)
        children[0].wait(timeout=10)
        assert children[1].poll() is None, "foreign daemon was stopped"
        module.kill_daemons(probe)
        (own / "refuse").touch()
        try: module.kill_daemons(probe)
        except RuntimeError as error: assert "native stop denied" in str(error)
        else: raise AssertionError("failed native cleanup was silently accepted")
        assert children[1].poll() is None, "failed cleanup touched the foreign daemon"
        for error in (FileNotFoundError("missing codex"), subprocess.TimeoutExpired("codex", 30)):
            with mock.patch.object(module.subprocess, "run", side_effect=error):
                try: module.kill_daemons(probe)
                except RuntimeError as caught: assert "stopping the synthetic Codex daemon failed" in str(caught)
                else: raise AssertionError("native stop exception was silently accepted")
    finally:
        for child in children:
            if child.poll() is None: child.terminate()
            child.wait(timeout=10)
events = []
fake_probe = types.SimpleNamespace(root=pathlib.Path("/synthetic"),
    stop=lambda: events.append("stop"), cleanup=lambda: events.append("cleanup"))
with mock.patch.object(module, "Probe", return_value=fake_probe), \
     mock.patch.object(module, "start_model_server", side_effect=RuntimeError("primary failure")), \
     mock.patch.object(module, "kill_daemons", side_effect=SystemExit("cleanup failure")), \
     mock.patch.object(module.subprocess, "run"):
    try: module.probe_codex()
    except RuntimeError as error: assert str(error) == "primary failure"
    else: raise AssertionError("primary failure was replaced or swallowed")
assert events == ["stop", "cleanup"], events
`
	if out, err := exec.Command("python3", "-c", script, source).CombinedOutput(); err != nil {
		t.Fatalf("synthetic daemon isolation: %v\n%s", err, out)
	}
}
