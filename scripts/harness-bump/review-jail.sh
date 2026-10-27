#!/usr/bin/env bash
# The jailed reviewer. JAIL_AUTH_DIR selects the local subscription
# profile. A trusted, empty-workspace Codex invocation refreshes its dedicated
# login before any PR content is mounted. Only that invocation may persist auth.
# The reviewer receives a disposable copy; its auth changes are never retained.
# No host-user configuration or history is mounted. Without JAIL_AUTH_DIR the
# API-broker regression fixture uses a dummy key and a credential-free jail.
# Both modes capture a verdict for the host, which alone holds the App key.
#
#   review-jail.sh            read the prompt on stdin, review, post
#   review-jail.sh --hostile  prove the jail leaks nothing, then exit
#   review-jail.sh --auth-probe  verify the dedicated login with no PR content
#
# Environment, all set by the workflow:
#   JAIL_IMAGE   the container image, pinned by digest
#   JAIL_NODE    a Node installation to mount at /opt/node
#   JAIL_CODEX   an npm prefix holding @openai/codex, mounted at /opt/codex
#   JAIL_GO      a Go installation to mount at /usr/local/go
#   JAIL_GOMOD   the host's Go module cache, mounted at /opt/gomod
#   JAIL_SOCKET  the broker's unix socket, mounted alone and read-only at
#                /run/broker/broker.sock: the jail's only way out, since
#                it has no network at all, and nothing it can unlink
#   JAIL_WORK    the pull request's worktree; the jail gets a copy
#   JAIL_BROKER  the broker's base URL as the container sees it, on its
#                own loopback where the dialer listens
#   JAIL_MODEL   the reviewer model
#   JAIL_EFFORT  the reviewer's reasoning effort (default medium)
#   JAIL_REPO    owner/name
#   JAIL_PR      the pull request number
#   JAIL_HEAD    the head commit the review is of; the verdict is posted
#                against exactly this commit (review mode)
#   JAIL_BASE    the immutable base commit (review mode)
#   JAIL_EVIDENCE optional bounded JSON supplementary evidence
#   JAIL_TOOL    the host's real noise-review
#   JAIL_BUMP    the host's noisedeck-harness-bump binary
set -euo pipefail

JAIL_EFFORT="${JAIL_EFFORT:-medium}"
for v in JAIL_IMAGE JAIL_NODE JAIL_CODEX JAIL_GO JAIL_GOMOD JAIL_SOCKET JAIL_WORK JAIL_BROKER JAIL_MODEL JAIL_REPO JAIL_PR JAIL_TOOL JAIL_BUMP; do
	[ -n "${!v:-}" ] || { echo "review-jail: ${v} is not set" >&2; exit 2; }
done
if [ "${1:-}" = "--auth-probe" ] && [ -z "${JAIL_AUTH_DIR:-}" ]; then
	echo 'review-jail: auth probe requires the dedicated subscription login' >&2
	exit 2
fi

jail="$(mktemp -d)"
container_name="noise-review-$(basename "${jail}")"
cleanup() {
	# Stop and reap the launcher before inventory: otherwise a delayed
	# registration could appear after an empty inventory and outlive us.
	local launcher
	for launcher in $(jobs -pr); do
		kill -KILL "${launcher}" 2>/dev/null || true
		wait "${launcher}" 2>/dev/null || true
	done
	stop_container || { echo 'review-jail: teardown uncertain; private scratch retained, authentication not promoted' >&2; return 1; }
	rm -rf "${jail}"
}
stop_container() {
	[ -f "${jail}/container.started" ] || return 0
	local inventory remaining
	inventory="$(docker ps -a --no-trunc --filter "name=${container_name}" --format '{{.ID}} {{.Names}}')" || return 1
	remaining="$(awk -v name="${container_name}" '$2 == name {print $1}' <<<"${inventory}")"
	if [ -n "${remaining}" ]; then
		[[ "${remaining}" =~ ^[0-9a-f]{64}$ ]] || return 1
		docker rm -f "${remaining}" >/dev/null || return 1
	fi
	inventory="$(docker ps -a --no-trunc --filter "name=${container_name}" --format '{{.ID}} {{.Names}}')" || return 1
	remaining="$(awk -v name="${container_name}" '$2 == name {print $1}' <<<"${inventory}")"
	[ -z "${remaining}" ] || return 1
	rm -f "${jail}/container.cid" "${jail}/container.started"
}
on_exit() {
	local status=$?
	trap - EXIT
	cleanup || exit 1
	exit "${status}"
}
trap 'on_exit' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
chmod 0755 "${jail}"

if [ -n "${JAIL_AUTH_DIR:-}" ]; then
	case "${JAIL_AUTH_DIR}" in /*) ;; *) echo 'review-jail: auth directory must be absolute' >&2; exit 2 ;; esac
	[ -d "${JAIL_AUTH_DIR}" ] && [ ! -L "${JAIL_AUTH_DIR}" ] || exit 2
	exec 9>"${JAIL_AUTH_DIR}/review.lock"
	flock -n 9 || { echo 'review-jail: subscription login is already in use' >&2; exit 2; }
	[ -f "${JAIL_AUTH_DIR}/auth.json" ] && [ ! -L "${JAIL_AUTH_DIR}/auth.json" ] || { echo 'review-jail: local Codex login is missing' >&2; exit 2; }
	jq -e '.auth_mode == "chatgpt" and (.tokens.refresh_token | type == "string" and length > 0) and (.tokens.access_token | type == "string" and length > 0) and (.OPENAI_API_KEY == null)' "${JAIL_AUTH_DIR}/auth.json" >/dev/null 2>&1 || { echo 'review-jail: a ChatGPT-managed login is required' >&2; exit 2; }
	install -d -m 0700 "${jail}/codex"
	install -m 0600 "${JAIL_AUTH_DIR}/auth.json" "${jail}/codex/auth.json"
fi

install -d "${jail}/work"
uid="$(id -u)"; gid="$(id -g)"

# The stub review command inside the jail: it records the verdict and
# posts nothing, because it can post nothing.
install -d -m 0755 "${jail}/bin"
cp "$(dirname "$0")/review-capture.sh" "${jail}/bin/noise-review"
chmod 0755 "${jail}/bin/noise-review"

# Only tool source files enter the jail. Missing libraries leave legacy body
# capture available; findings mode fails closed when it cannot load its parser.
install -d -m 0755 "${jail}/review-lib"
review_lib="$(dirname "$(dirname "${JAIL_TOOL}")")/lib"
if [ -f "${review_lib}/review.sh" ] && [ ! -L "${review_lib}/review.sh" ]; then
 for lib in review.sh utils.sh token.sh jwt.sh; do
  [ -f "${review_lib}/${lib}" ] && [ ! -L "${review_lib}/${lib}" ] || { echo 'review-jail: pinned review library is incomplete' >&2; exit 2; }
  install -m 0644 "${review_lib}/${lib}" "${jail}/review-lib/${lib}"
 done
fi

# The dialer's listen address: the broker URL without scheme and path.
dial="${JAIL_BROKER#http://}"
dial="${dial%%/*}"

run_jail() {
	local auth_args=(--env OPENAI_API_KEY=jail)
	local label_args=()
	if [ -n "${JAIL_SELINUX_LEVEL:-}" ]; then
		[[ "${JAIL_SELINUX_LEVEL}" =~ ^s0(:c[0-9]+(,c[0-9]+)*)?$ ]] || { echo 'review-jail: invalid proxy SELinux level' >&2; return 2; }
		label_args=(--security-opt "label=level:${JAIL_SELINUX_LEVEL}")
	fi
	if [ -n "${JAIL_AUTH_DIR:-}" ]; then
		auth_args=(--volume "${jail}/codex:/home/jail/.codex:Z" --env JAIL_SUBSCRIPTION=1 --env "HTTPS_PROXY=http://${dial}" --env "HTTP_PROXY=http://${dial}")
	fi
	# Private scratch gets this container's SELinux label. Read-only tools
	# and caches use a shared label so another job cannot relabel them away.
	touch "${jail}/container.started"
	docker run --rm --interactive --name "${container_name}" --cidfile "${jail}/container.cid" \
		--label "noise.review.probe=${JAIL_PROBE_ID:-none}" \
		--pids-limit 1024 --memory 6g --cpus 2 \
		--cap-drop ALL --security-opt no-new-privileges \
		"${label_args[@]}" \
		--user "${uid}:${gid}" \
		--tmpfs /tmp:rw,mode=1777 --tmpfs "/home/jail:rw,uid=${uid},gid=${gid}" \
		--env HOME=/home/jail "${auth_args[@]}" --env CODEX_HOME=/home/jail/.codex \
		--env PATH=/opt/node/bin:/opt/codex/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin \
		--env GOFLAGS=-mod=mod --env GOPATH=/home/jail/go --env GOCACHE=/home/jail/.cache/go-build \
		--env GOMODCACHE=/opt/gomod --env GOPROXY=off \
		--env "JAIL_HEAD=${JAIL_HEAD:-}" --env "JAIL_BASE=${JAIL_BASE:-}" \
		--env "JAIL_REPO=${JAIL_REPO}" --env "JAIL_PR=${JAIL_PR}" \
		--env JAIL_BROKER="${JAIL_BROKER}" --env JAIL_DIAL="${dial}" --env JAIL_MODEL="${JAIL_MODEL}" \
		--network none \
		--volume "${jail}/work:/work:Z" \
		--volume "${JAIL_SOCKET}:/run/broker/broker.sock:ro,Z" \
		--volume "${JAIL_BUMP}:/opt/broker/bump:ro,z" \
		--volume "${JAIL_NODE}:/opt/node:ro,z" \
		--volume "${JAIL_CODEX}:/opt/codex:ro,z" \
		--volume "${JAIL_GO}:/usr/local/go:ro,z" \
		--volume "${JAIL_GOMOD}:/opt/gomod:ro,z" \
		--volume "${jail}/bin/noise-review:/opt/broker/noise-review:ro,Z" \
		--volume "${jail}/review-lib:/opt/review-lib:ro,Z" \
		--workdir /work \
		"${JAIL_IMAGE}" sh -c 'mkdir -p "${CODEX_HOME}"; /opt/broker/bump jail-dial -listen "${JAIL_DIAL}" -socket /run/broker/broker.sock >/dev/null & exec "$@"' sh "$@" <&0 &
	wait $!
}

if [ "${1:-}" != "--hostile" ] && [ "${1:-}" != "--interrupt-probe" ]; then
	if [ "${1:-}" != "--auth-probe" ]; then
		[ -n "${JAIL_DIFF:-}" ] || { echo 'review-jail: immutable comparison diff is required' >&2; exit 2; }
		[ -n "${JAIL_HEAD:-}" ] && [ -n "${JAIL_BASE:-}" ] || { echo 'review-jail: immutable head and base are required' >&2; exit 2; }
		evidence_args=()
		if [ -n "${JAIL_EVIDENCE:-}" ]; then evidence_args=(-evidence "${JAIL_EVIDENCE}"); fi
		"${JAIL_BUMP}" review-prompt -diff "${JAIL_DIFF}" -repo "${JAIL_REPO}" -pr "${JAIL_PR}" \
			-head "${JAIL_HEAD}" -base "${JAIL_BASE}" "${evidence_args[@]}" >"${jail}/prompt"
	fi
	if [ -n "${JAIL_AUTH_DIR:-}" ]; then
		# No candidate files or candidate prompt are available to this run.
		# It uses only the fixed instruction and the dedicated subscription.
		run_jail codex exec --model "${JAIL_MODEL}" \
			--dangerously-bypass-approvals-and-sandbox --skip-git-repo-check \
			-c 'cli_auth_credentials_store="file"' -c 'forced_login_method="chatgpt"' \
			-c 'web_search=disabled' - <<<'Reply with OK. Do not use tools.' >"${jail}/refresh.out" 2>&1 || { echo 'review-jail: trusted Codex authentication refresh failed' >&2; exit 3; }
		stop_container || { echo 'review-jail: refresh workload termination is uncertain' >&2; exit 3; }
		[ -f "${jail}/codex/auth.json" ] && [ ! -L "${jail}/codex/auth.json" ] || exit 3
		[ "$(wc -c <"${jail}/codex/auth.json")" -le 65536 ] || exit 3
		jq -e '.auth_mode == "chatgpt" and (.tokens.refresh_token | type == "string" and length > 0) and (.tokens.access_token | type == "string" and length > 0) and (.OPENAI_API_KEY == null)' "${jail}/codex/auth.json" >/dev/null 2>&1 || exit 3
		# Promote only a closed credential shape from the trusted invocation.
		jq '{auth_mode, tokens: (.tokens | {id_token, access_token, refresh_token, account_id}), last_refresh}' "${jail}/codex/auth.json" >"${JAIL_AUTH_DIR}/auth.json.next"
		chmod 600 "${JAIL_AUTH_DIR}/auth.json.next"
		mv -f "${JAIL_AUTH_DIR}/auth.json.next" "${JAIL_AUTH_DIR}/auth.json"
		if [ "${1:-}" = "--auth-probe" ]; then
			echo 'review-jail: subscription authentication probe passed'
			exit 0
		fi
		# The PR gets no state or instructions from the refresh invocation.
		rm -rf "${jail}/codex" "${jail}/work"
		install -d -m 0700 "${jail}/codex"
		install -m 0600 "${JAIL_AUTH_DIR}/auth.json" "${jail}/codex/auth.json"
	fi
fi

# The reviewer sees only a disposable copy, with no host Git state.
cp -a "${JAIL_WORK}/." "${jail}/work"
rm -rf "${jail}/work/.git"

if [ "${1:-}" = "--interrupt-probe" ]; then
	run_jail sleep 300
	exit 0
fi

if [ "${1:-}" = "--hostile" ]; then
	# Effective state, not policy: from inside the jail, no credential is
	# readable in the environment, in any process, or on any mounted path,
	# and the host's process tree is not visible.
	run_jail sh -c '
		set -e
		leak() { echo "review-jail: hostile probe found $1" >&2; exit 1; }
		env | grep -Eq "GH_TOKEN|GITHUB_TOKEN|NOISE_REVIEW_(PRIVATE_KEY|APP_ID|INSTALLATION_ID)|HARNESS_BUMP_TOKEN" && leak "a credential in the environment"
		if [ "${JAIL_SUBSCRIPTION:-}" = 1 ]; then
			[ -z "${OPENAI_API_KEY:-}" ] || leak "an API key in subscription mode"
			[ -f /home/jail/.codex/auth.json ] || leak "missing subscription credential"
			[ "$(ls -A /home/jail/.codex | tr "\n" " ")" = "auth.json " ] || leak "host-user state beyond the subscription credential"
		else
			[ "$(printenv OPENAI_API_KEY)" = "jail" ] || leak "a real API key"
		fi
		for e in /proc/[0-9]*/environ; do { tr "\0" "\n" <"$e"; } 2>/dev/null | grep -Eq "GH_TOKEN=|CIHEAL_READ_TOKEN=|PRIVATE_KEY|HARNESS_BUMP_TOKEN=|sk-[A-Za-z0-9]{10}" && leak "a credential in $e"; done
		[ "$(ls /proc | grep -c "^[0-9]")" -lt 20 ] || leak "the host process tree"
		# A credential file: the app key by name anywhere, a private key by
		# content where one could have been mounted or written. The
		# toolchains carry certificate and test-fixture PEMs by name; those
		# are not keys the host holds.
		find / -xdev \( -name "app.pem" -o -name "auth.json" -o -name "hosts.yml" \) 2>/dev/null | { if [ "${JAIL_SUBSCRIPTION:-}" = 1 ]; then grep -v "^/home/jail/.codex/auth.json$"; else cat; fi; } | grep -q . && leak "a credential file by name"
		grep -rIl -- "-----BEGIN [A-Z ]*PRIVATE KEY-----" /run /home/jail /tmp /etc /opt/broker /opt/review-lib 2>/dev/null | grep -q . && leak "a private key"
		[ ! -e /run/secrets ] || leak "/run/secrets"
		[ ! -e /var/run/docker.sock ] || leak "the docker socket"
		# The broker socket is the only thing under /run/broker, and the
		# jail can neither remove it nor write beside it.
		[ "$(ls -A /run/broker | tr "\n" " ")" = "broker.sock " ] || leak "something beside the broker socket: $(ls -A /run/broker | tr "\n" " ")"
		rm -f /run/broker/broker.sock 2>/dev/null && leak "an unlinkable broker socket"
		touch /run/broker/x 2>/dev/null && leak "a writable broker mount"
		[ -S /run/broker/broker.sock ] || leak "the broker socket gone"
		grep -Eq "RUNNER_TEMP|review-home" /proc/mounts && leak "a host secret path in the mounts"
		test ! -w /opt/codex/bin/codex || leak "a writable reviewer binary"
		# The network: none. Loopback is the only interface, there is no
		# route and no resolver, so a name lookup cannot leave, let alone
		# carry bytes; the only way out is the dialer on loopback, which
		# reaches the broker socket and nothing else.
		[ "$(ls /sys/class/net | tr "\n" " ")" = "lo " ] || leak "an interface beyond loopback: $(ls /sys/class/net | tr "\n" " ")"
		[ "$(wc -l </proc/net/route)" -le 1 ] || leak "a route"
		curl --noproxy "*" -s --max-time 5 -o /dev/null https://api.openai.com/ && leak "a route to the internet"
		curl --noproxy "*" -s --max-time 5 -o /dev/null http://1.1.1.1/ && leak "a route to the internet by address"
		curl --noproxy "*" -s --max-time 5 -o /dev/null http://169.254.169.254/ && leak "the metadata service"
		getent hosts "exfil-$(head -c 8 /proc/sys/kernel/random/uuid).example.com" >/dev/null 2>&1 && leak "a resolver"
		broker="${JAIL_BROKER%/v1}"
		export NO_PROXY=127.0.0.1
		for _ in $(seq 1 50); do curl -s --max-time 1 -o /dev/null "${broker}/" && break; sleep 0.1; done
		if [ "${JAIL_SUBSCRIPTION:-}" = 1 ]; then
			for destination in github.com api.openai.com 127.0.0.1 169.254.169.254; do
				[ "$(curl --noproxy "" -s --max-time 5 -o /dev/null -w "%{http_connect}" --proxy "${broker}" "https://${destination}/")" = 403 ] || leak "an unauthorized proxy destination"
			done
		else
		[ "$(curl -s --max-time 5 -o /dev/null -w "%{http_code}" "${broker}/admin")" = "404" ] || leak "a broker that answers outside its one operation"
		[ "$(curl -s --max-time 5 -o /dev/null -w "%{http_code}" -X POST "${broker}/v1/files")" = "404" ] || leak "a broker that forwards another operation"
		[ "$(curl -s --max-time 5 -o /dev/null -w "%{http_code}" "${broker}/v1/responses")" = "405" ] || leak "a broker that forwards a GET"
		# The broker admits one request shape: another model, persistence
		# or a continuation is refused before the key is touched.
		[ "$(curl -s --max-time 5 -o /dev/null -w "%{http_code}" -X POST -H "content-type: application/json" -d "{\"model\":\"not-the-model\"}" "${broker}/v1/responses")" = "403" ] || leak "a broker that admits another model"
		[ "$(curl -s --max-time 5 -o /dev/null -w "%{http_code}" -X POST -H "content-type: application/json" -d "{\"model\":\"${JAIL_MODEL}\",\"store\":true}" "${broker}/v1/responses")" = "403" ] || leak "a broker that admits persistence"
		[ "$(curl -s --max-time 5 -o /dev/null -w "%{http_code}" -X POST -H "content-type: application/json" -d "{\"model\":\"${JAIL_MODEL}\",\"tools\":[{\"type\":\"web_search\"}]}" "${broker}/v1/responses")" = "403" ] || leak "a broker that admits a hosted tool"
		[ "$(curl -s --max-time 5 -o /dev/null -w "%{http_code}" -X POST -H "content-type: application/json" -d "{\"model\":\"${JAIL_MODEL}\",\"input\":[{\"role\":\"user\",\"content\":[{\"type\":\"input_image\",\"image_url\":\"https://x/y\"}]}]}" "${broker}/v1/responses")" = "403" ] || leak "a broker that admits an image or file input"
		[ "$(curl -s --max-time 5 -o /dev/null -w "%{http_code}" -X POST -H "content-type: application/json" -d "{\"model\":\"${JAIL_MODEL}\",\"input\":[{\"type\":\"function_call_output\",\"call_id\":\"c\",\"output\":[{\"type\":\"input_file\",\"file_url\":\"https://x/y\"}]}]}" "${broker}/v1/responses")" = "403" ] || leak "a broker that admits a file or URL inside a tool output"
		fi
		# The stub can record a verdict from inside: the one write the
		# jail must be able to make.
		/opt/broker/noise-review review --repo "$JAIL_REPO" --pr "$JAIL_PR" --event COMMENT --body "hostile probe" >/dev/null
		[ -s /work/.noise-verdict ] || leak "no way to record a verdict"
		echo "review-jail: hostile probe found nothing"
	' </dev/null
	# And the host can read what the jail recorded.
	"${JAIL_BUMP}" verdict -capture "${jail}/work/.noise-verdict" >/dev/null
	echo "review-jail: the verdict channel works from the jail to the host"
	exit 0
fi

# The prompt arrives on stdin and goes straight to the model; the
# container writes its transcript to a file the host never posts. Codex's
# hosted web search is off: the broker would refuse it, and the reviewer
# reads a pull request, not the web.
rm -f "${jail}/work/.noise-verdict"
rc=0
if [ -n "${JAIL_AUTH_DIR:-}" ]; then
	run_jail codex exec --model "${JAIL_MODEL}" -c "model_reasoning_effort=\"${JAIL_EFFORT}\"" \
		--dangerously-bypass-approvals-and-sandbox --skip-git-repo-check \
		-c 'cli_auth_credentials_store="file"' -c 'forced_login_method="chatgpt"' \
		-c 'web_search=disabled' - <"${jail}/prompt" >"${jail}/transcript" 2>&1 || rc=$?
else
run_jail codex exec --model "${JAIL_MODEL}" -c "model_reasoning_effort=\"${JAIL_EFFORT}\"" \
	--dangerously-bypass-approvals-and-sandbox --skip-git-repo-check \
	-c model_provider=jail -c 'model_providers.jail.name=jail' \
	-c "model_providers.jail.base_url=${JAIL_BROKER}" \
	-c 'model_providers.jail.env_key=OPENAI_API_KEY' -c 'model_providers.jail.wire_api=responses' \
	-c 'web_search=disabled' \
	- <"${jail}/prompt" >"${jail}/transcript" 2>&1 || rc=$?
fi
stop_container || { echo 'review-jail: review workload termination is uncertain' >&2; exit 3; }
if [ "${rc}" -ne 0 ]; then
	echo "review-jail: the reviewer exited ${rc}; inspect authentication or subscription availability on the private runner" >&2
	exit 3
fi
[ -s "${jail}/work/.noise-verdict" ] || { echo "review-jail: the reviewer recorded no verdict" >&2; exit 3; }

# The host validates what the jail captured and posts it as the app, the
# only step that holds the key. The body preserves the reviewed commits even
# if GitHub later reassociates commit_id after a force-push. The app token
# stays in the environment, never in an argv.
[ -n "${JAIL_HEAD:-}" ] || { echo "review-jail: JAIL_HEAD is not set" >&2; exit 2; }
"${JAIL_BUMP}" verdict -capture "${jail}/work/.noise-verdict" -head "${JAIL_HEAD}" -base "${JAIL_BASE}" -json \
	| jq --arg commit "${JAIL_HEAD}" '{commit_id: $commit, event: .event, body: .body}' >"${jail}/review.json"
# A moved base invalidates the assessment even if the head stayed unchanged.
refs="$(GH_TOKEN="$("${JAIL_TOOL}" token)" "${JAIL_BUMP}" review-refs -repo "${JAIL_REPO}" -pr "${JAIL_PR}")"
[ "$(jq -r .head <<<"${refs}")" = "${JAIL_HEAD}" ] && \
 [ "$(jq -r .base <<<"${refs}")" = "${JAIL_BASE}" ] || { echo 'review-jail: head or base moved during review; no verdict posted' >&2; exit 3; }
posted="$(GH_TOKEN="$("${JAIL_TOOL}" token)" gh api --method POST "repos/${JAIL_REPO}/pulls/${JAIL_PR}/reviews" --input "${jail}/review.json")"
[ "$(printf '%s' "${posted}" | jq -r .commit_id)" = "${JAIL_HEAD}" ] || { echo "review-jail: the posted review is not of ${JAIL_HEAD}" >&2; exit 3; }
printf '%s\n' "${posted}"
