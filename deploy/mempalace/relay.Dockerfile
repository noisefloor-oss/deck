# Standalone relay: native architecture, identical locked MemPalace to Deck.
FROM node:26.12.1-bookworm-slim@sha256:28147155d47dc31abe0a46e0d12b59b2fb3d0a2dcc5dc65fce76a074180b16fe
ARG UV_VERSION=0.13.5
ARG MEMPALACE_VERSION=3.10.0
# MemPalace PR 2631 until it lands in a release; see the Deck Dockerfile.
ARG MEMPALACE_COMMIT=bb01fd8c00bf5890111b67fd7783fddb48c5dcbe
COPY deploy/mempalace/security-uv.lock /tmp/security-uv.lock
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 python3-venv git ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && python3 -m venv /tmp/uv \
    && /tmp/uv/bin/pip install --disable-pip-version-check --no-input "uv==${UV_VERSION}" \
    && git init /tmp/source \
    && git -C /tmp/source remote add origin https://github.com/mempalace/mempalace.git \
    && git -C /tmp/source fetch --depth 1 origin refs/pull/2631/head \
    && git -C /tmp/source checkout --detach FETCH_HEAD \
    && test "$(git -C /tmp/source rev-parse HEAD)" = "${MEMPALACE_COMMIT}" \
    && cp /tmp/security-uv.lock /tmp/source/uv.lock \
    && UV_PROJECT_ENVIRONMENT=/opt/mempalace/venv /tmp/uv/bin/uv sync \
       --project /tmp/source --python /usr/bin/python3 --locked --no-dev --no-editable \
    && test "$(/opt/mempalace/venv/bin/mempalace --version)" = "MemPalace ${MEMPALACE_VERSION}" \
    && rm -rf /tmp/security-uv.lock /tmp/uv /tmp/source /root/.cache \
    && mkdir -p /data /home/relay \
    && chown 1003:1003 /data /home/relay
# The G3 root signed the compromised eMudhra intermediates.
RUN sed -i '\|^mozilla/emSign_ECC_Root_CA_-_G3.crt$|s|^|!|' /etc/ca-certificates.conf \
    && update-ca-certificates --fresh \
    && test ! -e /etc/ssl/certs/emSign_ECC_Root_CA_-_G3.pem

# Python clients use the same patched trust store as system TLS clients.
RUN ca_bundle="$(/opt/mempalace/venv/bin/python -c 'import certifi; print(certifi.where())')" \
    && ln -sf /etc/ssl/certs/ca-certificates.crt "${ca_bundle}" \
    && /opt/mempalace/venv/bin/python -c "import certifi, ssl; assert not any(value == 'emSign ECC Root CA - G3' for cert in ssl.create_default_context(cafile=certifi.where()).get_ca_certs() for rdn in cert['subject'] for key, value in rdn)"

COPY deploy/assert-tls-roots.mjs /tmp/assert-tls-roots.mjs
RUN node /tmp/assert-tls-roots.mjs && rm /tmp/assert-tls-roots.mjs

USER 1003:1003
ENV HOME=/home/relay MEMPALACE_CONFIG_DIR=/data MEMPALACE_MCP_IDLE_HOURS=0 MEMPALACE_SYNC_INTERVAL=2
EXPOSE 8766
ENTRYPOINT ["/bin/sh", "-ec", "test -n \"${MEMPALACE_MCP_HTTP_TOKEN:-}\" || { echo 'relay requires a bearer token' >&2; exit 78; }; exec /opt/mempalace/venv/bin/mempalace-mcp \"$@\"", "relay"]
CMD ["--palace", "/data/relay", "--transport", "http", "--host", "0.0.0.0", "--port", "8766"]
