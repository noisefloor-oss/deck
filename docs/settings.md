# Settings and environment variables

Choose these settings before the first build. The checked-in defaults do not
adapt to your host.

## Requirements

- A Linux host, or an Apple Silicon Mac with a local Linux ARM64 Docker
  VM, with Docker, the Compose plugin and BuildKit.
- An AVX2-class x86-64 CPU, or arm64.
- A host identity file at `/etc/machine-id` (standard on systemd-based
  Linux). On a Mac, see [Host identity](#host-identity).
- Accounts or subscriptions for the agent CLIs you want to use. Each CLI
  signs in through its own login flow.

## Installation settings

- `NOISEDECK_RUNTIME_UID` / `NOISEDECK_RUNTIME_GID` (default `1026:100`):
  the numeric Unix identity shared by the image build, the container
  process, the named-volume seed and the tmpfs mounts. Set both to the
  owner of the host directories mounted below. Choose them before first
  boot: persisted volume ownership and the host-binding receipt make a
  later change a data migration, not an environment-variable flip.
- `NOISEDECK_PROJECTS_HOST_PATH` (default: the checkout itself): the host
  directory mounted as `/workspace`, the projects root. The project
  picker lists only immediate non-dot directories, but agent CLIs can
  read everything accessible beneath the mount, including dotfiles and
  sibling projects. Point it at exactly what you intend to share.
- `NOISEDECK_GH_CONFIG_HOST_PATH` (default `~/.config/gh`): the host
  GitHub CLI configuration, mounted read-write into the shared provider
  home. Sessions inherit this GitHub authentication, and a token refresh
  or `gh auth login` inside a session writes back to the host directory.
  Prepare it writable by the chosen runtime identity.
- `NOISEDECK_SOURCE_ORG` (default `noisefloor-oss`): the GitHub
  organization the image build fetches Tasks, SDLC and Visual Diff from.
  Set it only when you install from a trusted fork or mirror; each pinned
  commit is still verified.
- `NOISEDECK_HOST_ID_PATH`: where the host identity file lives, for hosts
  that store it somewhere other than `/etc/machine-id`. It is not a way to
  invent a replacement identity.

The Compose file also forwards `ANTHROPIC_API_KEY` into the shared
runtime for task generation when it is set in the invoking shell or
environment file. With no key, task generation falls back to the bundled,
authenticated `claude` CLI. Leave the key unset unless you intend every
session to have that access. Exported shell values override the
environment file.

Authentication settings are in [Authentication](authentication.md), and
`NOISEDECK_PUBLIC_ORIGIN` is in [Serving beyond loopback](serving-beyond-loopback.md).

## Environment files

The production-profile example is
[deploy/compose.env.example](../deploy/compose.env.example). A local loopback
install needs no environment file if the defaults above fit your host.
Keep `NOISEDECK_PUBLIC_ORIGIN` unset for a loopback install.

To customize settings, create a private environment file containing only
the overrides you need, rather than copying the production profile
verbatim. Pass it on every command:

```sh
docker compose --env-file <path> up --build -d
```

Naming a file `deploy/compose.env` does not make Compose read it. An
environment file holding credentials belongs outside source control.

## Host identity

The durable data volume binds to the host identity file at
`/etc/machine-id` (standard on systemd-based Linux) on first boot.

On a Mac, use the actual IOPlatformUUID in a stable private file outside
the checkout and the projects directory, readable through approved Docker
file sharing. Never invent an identity. Follow the
[installer skill](../skills/install-deck/SKILL.md) for the private capture,
the explicit local Docker context and the mount checks.

Apple Silicon macOS with Colima has public-source build and browser
diagnostics, but not final end-to-end release qualification. Deck still
runs as Linux containers there. This does not qualify Docker Desktop,
Intel Macs, or native macOS Deck and provider hook behavior. For a native
Apple Silicon installation, use the
[native macOS installation procedure](../skills/install-deck/references/native-macos.md),
which has its own prerequisites and checks.

## What the build pulls

The image is built locally from this tree's Dockerfile. No prebuilt Deck
image is pulled or published. The build downloads its digest-pinned base
images, the version-pinned agent CLIs, and the pinned Noisefloor Tasks,
SDLC and Visual Diff sources named by the `NOISE_*_REPO` build arguments
in the Dockerfile. No GitHub token is needed.

Optional variants compose with the base file: a slim image carrying only
Claude Code and Codex (`-f compose.slim.yaml`), a Chromium sidecar for
browser automation (`-f compose.browser.yaml`), and derived images that
add project toolchains such as Go or Rust. See
[image variants and derived images](image-variants.md). Every variant is
built locally.

Every agent has the browser-screenshot skill, and with the Chromium
sidecar on, every session has `NOISE_BROWSER_WS` set so the skill can reach
the browser. Ask an agent for a screenshot of a page, such
as an app preview on `http://127.0.0.2:8840` or a public URL, and it saves
a PNG to the project's `.deck/assets/` folder, where you see it in the
Assets panel. The agent can also run the skill's script directly:

```sh
node /opt/noisedeck/skills/browser-screenshot/screenshot.mjs http://127.0.0.2:8840 home --width 390
```

Without the sidecar the skill says the browser overlay is not enabled and
takes no screenshot.

## The container and your sessions

Deck, tmux and the agent CLIs run in the same container. Stopping,
replacing or recreating it ends every tmux session and provider process.
The database and provider home persist in the named volumes `deck-data`
and `deck-home`, and project files persist in the host mount, but live
processes do not. Never run `docker compose down -v` unless you intend to
delete those volumes.

## Updating

Update the source checkout and rebuild with the same commands and the
same settings:

```sh
docker compose up --build -d
```

Keep the same runtime user and group, host identity and volumes. The
database migrates at startup. Take a [backup](backup-restore.md) before you
update.

## Agent chat and shared memory

Agents can message each other and keep shared memory through MemPalace. It
is an optional overlay, off by default, and the tutorial lesson on two
agents working together needs it.

On Docker, add `compose.mempalace.yaml` to your Compose command and point
`NOISEDECK_MEMPALACE_HOST_PATH` at a directory for the memory store. Create
the directory first, owned by your runtime user:

```sh
NOISEDECK_MEMPALACE_HOST_PATH=$HOME/.mempalace \
  docker compose -f compose.yaml -f compose.mempalace.yaml up --build -d
```

Recreating the container ends running sessions, as in
[The container and your sessions](#the-container-and-your-sessions).
The native macOS procedure does not include agent chat. How the chat works
is in [Agent-to-agent chat](a2a-chat.md).

## Conversation attention

Pi sessions can show **Agent needs input** when a native blocking dialog is
open, or **Unseen ended turn** after an agent run settles. An ended turn does
not mean the requested task succeeded. Opening the conversation acknowledges
its ended-turn marker once the terminal or recovery transcript is visible;
a pending input request stays marked until the native dialog closes.

The current integration is qualified for Pi 1.4.0. It uses `ui_prompt_start`,
`ui_prompt_end` and `agent_settled`, including native queued continuations.
It does not treat an intermediate `agent_end`, quiet terminal, tool output or
transcript text as completion. Other harnesses currently have no conversation
attention integration and remain unknown. Existing output activity, recovery
and agent-chat indicators retain their separate meanings.

Attention facts and view receipts survive browser reloads and backend
restarts. They are tied to the exact process generation and native
conversation, and the sidebar receives them on its normal session refresh
(about every ten seconds). A hidden tab or a covering dialog does not mark a
turn seen. Only lifecycle metadata is stored; native prompts, dialog titles,
messages and tool arguments are not collected for badges.

## Public release notices

A stable release installation checks GitHub's public Deck release metadata
when the workspace first opens, then reuses successful results for six hours. The server sends no account credential,
installation identifier or workspace data. Development, commit and prerelease
builds do not check. An offline or rate-limited check leaves the workspace alone
and the server can retry after fifteen minutes on a new request. An open
browser checks every six hours, so recovery from an offline first check can
take that long to appear unless you reload the page.

When a newer stable release exists, the info bar links to its release notes.
The **Show info bar** preference applies. On phones the bar appears only while
a release notice is available.
Dismiss hides that release in this browser; a later release can appear again.
The notice never installs an update or reloads the workspace. The separate
**New version available** header button still means the running server changed
since this browser tab loaded, and reloads only when you click it.
