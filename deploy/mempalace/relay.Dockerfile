# Standalone relay: native architecture, identical locked MemPalace to Deck.
FROM cgr.dev/chainguard/wolfi-base@sha256:0feab531471d2edfcca55f18883441cb7df598e724fa2dedf9985273f532eed7 AS relay-build
ARG UV_VERSION=0.13.5
ARG MEMPALACE_VERSION=3.11.0
ARG MEMPALACE_COMMIT=602d796c699cabba2971982cd9dccbbf46abfe79
COPY deploy/mempalace/security-uv.lock /tmp/security-uv.lock
COPY deploy/mempalace/sqlite-only.patch /tmp/sqlite-only.patch
COPY deploy/mempalace/migrate-to-sqlite.py /tmp/migrate-to-sqlite.py
COPY deploy/mempalace/test_migrate_to_sqlite.py /tmp/test_migrate_to_sqlite.py
RUN apk upgrade --no-cache \
    && apk add --no-cache python-3.14 py3.14-pip git ca-certificates-bundle \
    && python3.14 -m venv /tmp/uv \
    && /tmp/uv/bin/pip install --disable-pip-version-check --no-input "uv==${UV_VERSION}" \
    && git init /tmp/source \
    && git -C /tmp/source remote add origin https://github.com/mempalace/mempalace.git \
    && git -C /tmp/source fetch --depth 1 origin "${MEMPALACE_COMMIT}" \
    && git -C /tmp/source checkout --detach FETCH_HEAD \
    && test "$(git -C /tmp/source rev-parse HEAD)" = "${MEMPALACE_COMMIT}" \
    && git -C /tmp/source apply /tmp/sqlite-only.patch \
    && cp /tmp/security-uv.lock /tmp/source/uv.lock \
    && UV_PROJECT_ENVIRONMENT=/opt/mempalace/venv /tmp/uv/bin/uv sync \
       --project /tmp/source --python /usr/bin/python3.14 --locked --no-dev --no-editable \
    && PYTHONDONTWRITEBYTECODE=1 /opt/mempalace/venv/bin/python -m unittest discover -v -s /tmp -p test_migrate_to_sqlite.py \
    && /opt/mempalace/venv/bin/python -c 'import importlib.util; assert importlib.util.find_spec("chromadb") is None' \
    && test "$(/opt/mempalace/venv/bin/mempalace --version)" = "MemPalace ${MEMPALACE_VERSION}" \
    && install -m 0555 /tmp/migrate-to-sqlite.py /usr/local/lib/migrate-mempalace-to-sqlite.py \
    && rm -rf /tmp/security-uv.lock /tmp/sqlite-only.patch /tmp/migrate-to-sqlite.py /tmp/test_migrate_to_sqlite.py /tmp/uv /tmp/source /root/.cache
FROM cgr.dev/chainguard/wolfi-base@sha256:0feab531471d2edfcca55f18883441cb7df598e724fa2dedf9985273f532eed7
RUN apk upgrade --no-cache \
    && apk add --no-cache python-3.14 ca-certificates-bundle libgomp libstdc++
COPY --from=relay-build /opt/mempalace/venv /opt/mempalace/venv
COPY --from=relay-build /usr/local/lib/migrate-mempalace-to-sqlite.py /usr/local/lib/migrate-mempalace-to-sqlite.py
COPY deploy/mempalace/relay-entrypoint.sh /usr/local/bin/relay-entrypoint
RUN chmod 0555 /usr/local/bin/relay-entrypoint \
    && mkdir -p /data /home/relay \
    && chown 1003:1003 /data /home/relay
COPY deploy/mempalace/patch-python-tls-roots.py /tmp/patch-python-tls-roots.py
RUN /opt/mempalace/venv/bin/python /tmp/patch-python-tls-roots.py \
    && rm /tmp/patch-python-tls-roots.py \
    && ! command -v node \
    && ! command -v npm \
    && ! command -v git

USER 1003:1003
ENV HOME=/home/relay MEMPALACE_CONFIG_DIR=/data MEMPALACE_BACKEND=sqlite_exact MEMPALACE_MCP_IDLE_HOURS=0 MEMPALACE_SYNC_INTERVAL=2
EXPOSE 8766
ENTRYPOINT ["/usr/local/bin/relay-entrypoint"]
CMD ["--palace", "/data/relay", "--transport", "http", "--host", "0.0.0.0", "--port", "8766"]
