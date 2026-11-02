---
name: install-deck
description: Install Noisefloor Deck from its latest public release with Docker Compose on macOS or Linux. Checks your system, proposes one plan, runs it after one go-ahead, and verifies that Deck works. Use when you ask to install or reinstall Deck; to update a running Deck, use update-deck.
---

# Install Noisefloor Deck

Deck runs as Docker containers on Linux or an Apple Silicon Mac. Native macOS
is coming; Docker is the supported path today, on a Mac too. The install is
done when you can use Deck (step 5), not when the build finishes.

## Rules that protect you

- The agent never runs `sudo`. Anything that needs admin rights goes in the
  plan as a command for you to run.
- The agent never reads, stores or relays a password. Tool output is kept in
  transcripts, so you type it yourself. The agent reads your one-time setup
  link once, at the end, and shows it to you. The link works once, expires
  30 minutes after Deck creates it and stops working when a password exists.
- Deck listens on 127.0.0.1 only. The agent never widens the port binding or
  changes `NOISEDECK_AUTH_MODE`. It sets `NOISEDECK_PUBLIC_ORIGIN` only to your
  tailnet address, and only after you say yes to the Tailscale option.
- The agent never runs `tailscale funnel` or puts Deck on the public internet.
  Deck runs agents with shell access, so if you ask for public access, the agent
  explains that and stops.
- The agent never deletes, resets or re-owns the `deck-data` and `deck-home`
  volumes or the folders Deck mounts, and never runs `docker compose down -v`.
- A failed check is reported with its fix, never worked around with another
  version, weaker settings or a made-up identity.

## 1. Working mode

Unless you asked to go step by step, investigate the system and propose a
plan without an opening question. State that choice in the plan and wait for
your go-ahead before making changes. Step-by-step mode runs the same checks
but pauses at each plan line and each action.

## 2. Pre-check (read-only, no questions)

- **OS and CPU.** `uname -s`, `uname -m`. Supported: Linux on arm64 or on
  x86-64 with AVX2 (`grep -qw avx2 /proc/cpuinfo`), and Apple Silicon Macs.
- **Docker.** Record `docker context show` as CTX; every `docker` command
  from here on, Compose included, is `docker --context CTX`. Run `version`,
  `compose version` and `buildx version`. Tell apart not installed,
  installed but not running (the client answers, the server does not) and,
  on Linux, `permission denied` on the socket. The engine must be local:
  `docker --context CTX context inspect CTX --format '{{.Endpoints.docker.Host}}'`
  is `unix://` and `info --format '{{.OSType}} {{.Architecture}}'` is
  `linux` on this CPU.
- **Host identity.** Linux: `/etc/machine-id` exists and is not empty. Mac:
  whether the identity file from the plan already exists.
- **Existing Deck**, across all Compose projects, stopped ones included:
  `docker volume ls --filter label=com.docker.compose.volume=deck-data --format '{{.Name}} {{.Label "com.docker.compose.project"}}'`,
  the same for `deck-home`, and
  `docker ps -a --filter label=com.docker.compose.service=app --format '{{.ID}} {{.Label "com.docker.compose.project"}}'`.
  An install is a project with both volumes mounted by its app container
  (`docker inspect --format '{{json .Mounts}}' ID`); any other `app` is not Deck.
- **Ports.** `curl -s -o /dev/null --max-time 2 http://127.0.0.1:18082/` exits
  7 when free; agent chat also needs 18766.
- **Disk.** Run `docker --context CTX system df` for Docker's current usage;
  it does not report free space. On Linux, run `df -h` on the directory from
  `docker --context CTX info --format '{{.DockerRootDir}}'`. On a Mac, check
  the filesystem holding Docker Desktop's disk image, normally
  `~/Library/Containers/com.docker.docker/Data/vms/0/data/Docker.raw`, with
  `df -h` on its parent directory. If the image is elsewhere, use its location
  in Docker Desktop's Settings > Resources > Advanced. Check the configured
  disk usage limit there too; host free space alone does not establish room
  in the VM. A full build needs roughly 20 GB. Do not prune images or volumes
  to make room without including that in the plan.
- **Agents.** For claude, codex, kimi, opencode, cursor-agent, qwen, crush,
  pi, omp and devin: installed (`command -v`) and signed in (its config
  folder exists in your home; never open it).
- **Battery, SSH, display.** Sessions run on this machine, so it matters
  whether it sleeps or shuts down. Record only what you see: a battery (Linux:
  a `/sys/class/power_supply/BAT*` folder; Mac: `pmset -g batt` lists an
  internal battery), whether you were reached over SSH (`SSH_CONNECTION` set),
  and on Linux whether a display exists (`DISPLAY` or `WAYLAND_DISPLAY` set).
  None of these says the machine stays on; don't infer that.
- **Tailscale.** Read-only, as in [references/tailscale.md](references/tailscale.md):
  whether the command exists, `tailscale status --json` for sign-in, this
  machine's name and HTTPS certificates.
- **Shell overrides.** Names only:
  `awk 'BEGIN { for (k in ENVIRON) if (k ~ /^NOISE/) print k }'`.
- **Release.** The latest stable `vX.Y.Z` from
  `https://api.github.com/repos/noisefloor-oss/deck/releases/latest` (or the
  fork you named), with no credentials. Every networked `git` is `GIT`. It
  runs in a new empty folder, `T=$(mktemp -d)`, never inside a repository,
  and ignores all Git config, prompts, redirects and Git variables (behind a
  proxy, also pass `HTTPS_PROXY`). Its arguments replace `…`:
  `(cd "$T" && env -i PATH="$PATH" GIT_CEILING_DIRECTORIES="${T%/*}" GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 git -c http.followRedirects=false …)`.
  Prove the tag's commit with
  `GIT ls-remote https://github.com/OWNER/REPO.git 'refs/tags/vX.Y.Z' 'refs/tags/vX.Y.Z^{}'`:
  the `^{}` line if present, else the tag line, one 40-character SHA.

## 3. The plan

One message. Every line carries a default you can change. No acceptance
steps; state facts where they change a choice.

- **What Deck is for** (several allowed):
  - building software: Go toolchain (`compose.go.yaml`), browser sidecar
    (`compose.browser.yaml`), project previews (`compose.previews.yaml`,
    Linux only; a Mac first needs loopback aliases for 127.0.0.2 to 127.0.0.9);
  - agents working together: agent chat and shared memory
    (`compose.mempalace.yaml`), recommended yes;
  - other work: lite Deck.
- **Full or lite**, from the purpose and agents. Full has every agent and is
  needed for the Go toolchain and agent chat; lite (`compose.slim.yaml`) has
  Claude Code and Codex, and works with the browser sidecar and previews.
- **Agents.** Pre-tick what the pre-check found. You sign in to each inside
  Deck once; logins on this machine do not carry over.
- **Paths**, none inside the projects folder: checkout `~/noisedeck/deck`;
  env file `~/noisedeck/deck.env`; projects `~/noisedeck/projects` ("every
  agent can read everything in this folder, so put in it only what you want
  shared"); Deck's GitHub login `~/noisedeck/gh`; memory store
  `~/noisedeck/memory` with agent chat; Mac identity `~/noisedeck/host-id`.
- **Runtime user** `id -u`:`id -g`, so Deck's files are yours (not root).
- **Source organization**, only if you named a fork you trust: its
  organization is OWNER, `NOISEDECK_SOURCE_ORG=OWNER` goes in the env file,
  and Deck, a2a, Tasks, SDLC and Visual Diff all come from it, at
  the commits Deck pins. Otherwise `noisefloor-oss`.
- **Port** 18082, or the next free one. **Version** `vX.Y.Z` and its commit.
- **What you run first**, only what is missing:
  - Mac without Docker: `brew install --cask docker`, then open Docker once.
    Installed but stopped: the agent runs `open -a Docker` after the go-ahead.
  - Linux without Docker: `curl -fsSL https://get.docker.com | sudo sh`
    (or <https://docs.docker.com/engine/install/>). Stopped:
    `sudo systemctl start docker`. No permission: `sudo usermod -aG docker "$USER"`,
    then sign out and in. No Compose or Buildx: install
    `docker-compose-plugin docker-buildx-plugin` with `apt-get` or `dnf`.
  - No `/etc/machine-id`: `sudo systemd-machine-id-setup`.
- **Where Deck runs**, from the pre-check. Say: "Deck is built for a machine
  that stays on. Sessions keep running when you close the browser tab, but
  pause if this machine sleeps and stop if it shuts down." Then one sentence
  for each signal you saw. A battery: "this looks like a laptop, so expect that
  when you close the lid; good for trying Deck". Reached over SSH or no
  display: "this looks like a remote or headless machine; if you keep it awake,
  sessions keep going, and the Tailscale option below lets you reach it from
  your laptop and phone". Never say the machine stays on. Never refuse or pause
  the install for this.
- **Tailscale**, when found or when you asked for remote access: offer "Open
  Deck from your other devices over Tailscale (your tailnet only)" with the
  exact address. Recommend yes when it looks remote or headless, no when it has
  a battery. Anything missing is a step for you. Project previews only open on
  the Deck machine and cannot run with this option, so say so and let you
  choose. Details in [references/tailscale.md](references/tailscale.md).
- **Shell overrides** found: they beat the env file, so `DC` unsets each.
- **Restart check**: once, after you sign in and create a project, before
  any session starts.
- **Existing Deck.** Report its version and offer to keep it or
  [update it](#update); its app's labels `com.docker.compose.project.`
  `working_dir`, `environment_file` and `config_files` give its checkout,
  env file and overlays. Keep: check `exec ID noise-deck version` and that
  `inspect --format '{{.State.Health.Status}}' ID` is `healthy`, then stop
  without recreating anything. Volumes without a matching app, or several
  installs, are yours to sort out; never create replacement volumes.

## 4. One go-ahead, then run it end to end

Run every step without further questions (step by step: pause before each).

1. `mkdir -p` the plan's folders.
2. `GIT clone --branch vX.Y.Z --depth 1 https://github.com/OWNER/REPO.git DIR`,
   with DIR an absolute path that does not exist yet;
   `git -C DIR rev-parse HEAD` must equal the proved SHA.
3. Mac: `sh DIR/skills/install-deck/scripts/macos-host-id.sh HOST_ID_FILE`
   writes the Mac's hardware ID privately and prints nothing.
4. Write the env file, mode 600, absolute paths, only these `NOISEDECK_`
   keys: `BUILD_VERSION` and `IMAGE_TAG` (both `X.Y.Z`), `RUNTIME_UID`,
   `RUNTIME_GID`, `PROJECTS_HOST_PATH`, `GH_CONFIG_HOST_PATH`, and as needed
   `HOST_ID_PATH`, `MEMPALACE_HOST_PATH`, `HOST_PORT`, `SOURCE_ORG` (a named
   fork only), `PUBLIC_ORIGIN` (the Tailscale option only).
5. Every Compose command uses one prefix, `DC` below:
   `env -u NAME… docker --context CTX compose --project-directory DIR --env-file ENV -p deck -f DIR/compose.yaml`
   (one `-u` per shell override; none, no `env`) plus `-f DIR/<overlay>`
   in this order, each only if chosen: `compose.slim.yaml` (lite);
   `compose.mempalace.yaml`, `compose.go.yaml`, `compose.go-mempalace.yaml`
   (when both of those are on), all full only; `compose.browser.yaml`,
   `compose.previews.yaml`.
6. `DC up --build -d`. Wait for `DC ps` to show app healthy;
   `DC exec -T app noise-deck version` must print `X.Y.Z`.
7. `docker --context CTX inspect --format '{{.Config.User}}{{range .Mounts}}{{if eq .Destination "/workspace"}} {{.Source}}{{end}}{{end}}{{range .Config.Env}}{{if eq . "NOISEDECK_AUTH_MODE=password"}} password{{end}}{{end}}' "$(DC ps -q app)"`
   must print the plan's `UID:GID`, projects path and `password` (Docker
   Desktop may prefix the path with `/host_mnt`).
8. Tailscale option only: set up and check it as in
   [references/tailscale.md](references/tailscale.md), before step 5.

## 5. Done, checked by the agent (report each result)

1. `curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:PORT/` answers.
2. `DC exec -T app noise-deck doctor` reports no required failures. A release
   without `doctor`: skip and say so.
3. You create your password. Read the setup link once, with
   `DC exec -T app cat /data/setup-url`, and say: "Open this link to create
   your password. It works once and expires 30 minutes after it was created: LINK". Do not keep
   it. The link carries Deck's configured address, so after changing the port
   or origin, run `DC up -d` and wait for healthy first. If the file is
   absent, a password already exists: point you to
   `http://127.0.0.1:PORT/` (your tailnet address, with the Tailscale option)
   to sign in. If the link has expired, opening it renews it; read the
   file again for the new link. A release
   without first-run password creation has no `setup-url` and prints a
   one-time password in the logs: ask you to run `DC logs app` (full command)
   in your own terminal and enter that password on the sign-in page. The
   agent never reads or relays it. With the Tailscale option the link is your
   tailnet address and opens only on your tailnet devices.
4. You choose Connect GitHub and finish it. Check:
   `DC exec -T app gh auth status >/dev/null 2>&1 && echo signed-in`.
5. You create a project. Check its folder exists.
6. Fresh install only, with no session started yet: `DC restart`. Wait for
   healthy, repeat check 1, and ask you to reload: you are still signed in
   and your project is still there.
7. You start a session, sign in to your chosen agent CLI in it and give it
   one tiny task, such as "reply with OK". The agent asks you to confirm it
   answered; your yes is the check.

Report a failure with its step, its output (no secrets) and the fix.

## Update

For an existing installation, give your coding agent this prompt:

> Update my Noisefloor Deck to its latest release, following
> <https://github.com/noisefloor-oss/deck/blob/main/skills/update-deck/SKILL.md>.

The agent proposes a plan and waits for your go-ahead. It backs up Deck
and keeps the old version so you can roll back, then installs the update and
checks that Deck works. Updating ends running sessions, so you choose when.

Only when you choose it, follow the
[update skill](../update-deck/SKILL.md) instead of the install steps above.
