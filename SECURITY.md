# Security policy

## Report a vulnerability

Use [GitHub private vulnerability reporting](https://github.com/noisefloor-oss/deck/security/advisories/new).
Do not include vulnerabilities, credentials or private project data in
public issues.

Deck is experimental and solo-maintained. There is no response-time
guarantee or bug bounty. Only the latest tagged public release is
supported; earlier releases do not receive security backports. Security
fixes for the current public release are published as public patch
releases.

## Deployment boundary

Deck runs native coding-agent command-line tools. Its shared-workspace
deployment is unconfined: a project directory, a session or a container
does not establish isolation between agents. Sessions share the projects
bind mount, the provider home and native history, host GitHub CLI
authentication, one Unix identity, one PID/network environment and one
tmux namespace. An agent can read sibling projects, dotfiles and private
environments beneath the mount, and may use ambient remote credentials
beyond its selected working directory. See the
[deployment and access description](README.md#your-accounts-and-files)
before supplying private data or credentials.

Report failures of the documented browser authentication and
origin checks, or other behavior that contradicts the documented access
boundaries. Reports outside those boundaries are also welcome, but may be
treated as hardening requests rather than vulnerabilities.
Authentication, browser access controls and process lifecycle checks do
not by themselves establish agent confinement.
