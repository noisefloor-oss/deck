# Noisefloor Deck

Deck is an opinionated workspace for building products with AI writing the
code. You set the direction and the quality standard, and work with coding
agents from across frontier labs, using both closed and open-weight models.

Deck brings your projects, tasks and agent sessions into one place in your
browser. A project can involve Claude Code, Codex, Kimi or other supported
agents you choose for the work, with one implementing a change and another
reviewing it before you try the result.

Deck was built using Deck, and its tools and workflow grew out of the work
of developing, testing and releasing the product you see here.

![A Claude Code session open, the projects sidebar on the left and the Tasks panel docked on the right](docs/assets/deck.png)

## A project, from brief to release

The work starts with what you want to build. A brief becomes a set of tasks,
and each agent takes on a piece of the project in its own working copy.
You can discuss a design, change priorities or ask for another approach
as the product takes shape.

The workflow asks agents to check their work through tests, review by a
different model and screenshot comparisons against the design. As they work
through those checks and address the findings, you can concentrate on how
the product behaves and whether it is ready to ship.

## Tools built for agents

Deck brings together small [Noisefloor](https://noisefloor.sh) tools, each
with a specific job. You can also use them outside Deck.

- [Tasks](https://github.com/noisefloor-oss/tasks) turns a brief into a
  backlog agents can work through. It tracks dependencies and ownership,
  and manages each task's working copy from creation through cleanup.
- [Visual Diff](https://github.com/noisefloor-oss/visual-diff) compares
  screenshots of the running product with its design. Agents use the
  differences to adjust the implementation and measure whether it improved.
- [SDLC](https://github.com/noisefloor-oss/sdlc) gives a project shared rules
  for implementation, review and delivery, including which agents fill each
  role. It records those instructions and checks whether they have changed.

The tools provide a way for agents to follow the workflow; Deck does not
assign or move work between them on its own. Optional messaging and shared
memory let you ask agents to work together across sessions. The in-app
tutorial walks through the workflow on a project.

## Getting started

Give your coding agent this prompt:

> Install Noisefloor Deck from its latest release, following
> <https://github.com/noisefloor-oss/deck/blob/main/skills/install-deck/SKILL.md>.

The agent checks the machine where Deck will run and proposes a plan.
After your go-ahead it installs Deck and checks that it works. You run
anything that needs admin rights, then open the setup link and choose your
password. The [manual install guide](docs/install.md) has the steps if you
prefer to do it yourself.

## Your projects away from your desk

Deck works best on a Linux machine that stays on, such as a home server or
cloud machine. Your sessions run there, so they keep running when you close
the tab, disconnect from Deck or shut down your laptop. Deck also runs on
your laptop; there, sessions pause when it sleeps and stop when it shuts down.

The installer can give your laptop and phone access over a private Tailscale
network, without making Deck public. You can start a refactor at your desk,
then check the result or answer the agent's question from your phone. The
[remote and phone guide](docs/remote-and-mobile.md) has the steps.

## Your accounts and files

Deck runs the agents' own command-line tools with your existing accounts,
history and approvals; it does not proxy or resell models. Deck is built for
one person: sign-in protects your browser's access, and there are no user
accounts or roles. Deck is not a sandbox. The agents share one account on
the machine, so anything one of them can reach, all of them can, including
other projects, saved logins and private files
in the shared folders. Run Deck on a machine you control and share only the
folders you mean to. Deck is experimental, so interfaces, file formats and
behavior may change between releases.

## Harness support

These are the pins in the full Docker image. The slim image includes Claude
Code and Codex. Crush is bundled as a CLI, but has no Deck conversation picker.

<!-- harness-pins:start -->
| Harness | Pinned version | Pinned on (UTC) | Agent chat | Login helper | Attention badges |
| --- | --- | --- | --- | --- | --- |
| `claude` | `2.1.311` | 2026-10-27 | Live | None | Unknown |
| `codex` | `0.162.1` | 2026-10-10 | Live | Browser callback | Unknown |
| `kimi` | `2.3.3` | 2026-10-27 | Next turn | None | Unknown |
| `opencode` | `1.19.4` | 2026-10-23 | Live | None | Unknown |
| `command-code` | `1.88.0` | 2026-10-23 | Manual | None | Unknown |
| `cline` | `3.0.74` | 2026-10-27 | Manual | None | Unknown |
| `kilo` | `7.8.18` | 2026-10-27 | Manual | None | Unknown |
| `qwen` | `0.26.1` | 2026-10-25 | Manual | None | Unknown |
| `mimo` | `0.1.16` | 2026-10-27 | Manual | None | Unknown |
| `qoder` | `1.1.79` | 2026-10-27 | Manual | None | Unknown |
| `cursor-agent` | `2026.10.22-27dcf5f` | 2026-10-23 | Manual | None | Unknown |
| `devin` | `3000.11.205` | 2026-10-23 | Manual | None | Unknown |
| `grok` | `1.0.70` | 2026-10-25 | Manual | None | Unknown |
| `omp` | `18.13.0` | 2026-10-27 | Live | None | Unknown |
| `pi` | `1.4.0` | 2026-10-22 | Live | ChatGPT, Anthropic | Input, ended turn |
| `crush` | `0.98.2` | 2026-10-23 | Manual | None | Unknown |
<!-- harness-pins:end -->

Pinned on records when Deck adopted that version, not its upstream release
date or the last README edit. Pin refreshes update the version and date together;
re-adopting an older pin also records a new date.

For [agent chat](docs/a2a-chat.md), Live means a native delivery path can wake
an idle agent; Next turn means delivery waits for a native hook. Manual means
the agent checks its inbox. The login helper
handles the listed remote login flows; None means use the CLI’s native flow.
[Attention badges](docs/settings.md#conversation-attention) use qualified native
events. Unknown means Deck does not infer that harness’s input or turn state.

Agent chat requires the Docker setup; the native macOS source procedure does
not include it. Native accounts, approvals and model access stay with each CLI.

## More

- [Settings](docs/settings.md), [authentication](docs/authentication.md)
  and [serving beyond loopback](docs/serving-beyond-loopback.md)
- [Remote box, Tailscale and phone](docs/remote-and-mobile.md)
- [Agent chat and shared memory](docs/settings.md#agent-chat-and-shared-memory)
- [Backup and restore](docs/backup-restore.md) and [uninstall](docs/uninstall.md)
- [Project tasks](docs/project-tasks.md), [project previews](docs/project-previews.md)
  and [Connect GitHub](docs/github-connection.md)
- [Fonts](docs/fonts.md) and [building from source without Docker](docs/building-from-source.md)

## Links

- Deck is part of [Noisefloor](https://noisefloor.sh).
  [DEVELOPMENT.md](DEVELOPMENT.md) explains how this repository is developed.
- Report bugs in [Issues](https://github.com/noisefloor-oss/deck/issues).
  Report vulnerabilities privately as [SECURITY.md](SECURITY.md) describes.
- Licensed under the [MIT License](LICENSE). Bundled third-party components
  keep their own licenses; see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
  Licensed fonts you supply yourself are not covered by that license.
