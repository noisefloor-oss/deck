#!/bin/sh
set -eu

image="${NOISE_BROWSER_TEST_IMAGE:-noise-browser-acceptance:local}"
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(dirname -- "${script_dir}")

if test "${NOISE_BROWSER_SKIP_BUILD:-0}" = 1; then
  docker image inspect "${image}" >/dev/null
  test "$(docker image inspect "${image}" --format '{{.Os}}/{{.Architecture}}')" = linux/amd64
else
  docker build --target browser --tag "${image}" "${repo_dir}"
fi
docker run --rm --init \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --tmpfs /tmp:rw,exec,nosuid,nodev,size=512m \
  "${image}" /bin/sh -c '
    set -eu
    tls_pki=$(mktemp -d /tmp/deck-tls.XXXXXX)
    openssl req -x509 -newkey rsa:2048 -nodes \
      -keyout "${tls_pki}/ca.key" -out "${tls_pki}/ca.crt" \
      -days 1 -subj "/CN=Deck acceptance CA"
    openssl req -newkey rsa:2048 -nodes \
      -keyout "${tls_pki}/server.key" -out "${tls_pki}/server.csr" \
      -subj "/CN=localhost"
    printf "%s\n" \
      "basicConstraints=critical,CA:FALSE" \
      "keyUsage=critical,digitalSignature,keyEncipherment" \
      "extendedKeyUsage=serverAuth" \
      "subjectAltName=DNS:localhost,IP:127.0.0.1" \
      >"${tls_pki}/server.ext"
    openssl x509 -req -in "${tls_pki}/server.csr" \
      -CA "${tls_pki}/ca.crt" -CAkey "${tls_pki}/ca.key" -CAcreateserial \
      -out "${tls_pki}/server.crt" -days 1 -extfile "${tls_pki}/server.ext"
    certutil -A -d "sql:${HOME}/.pki/nssdb" -n deck-acceptance-ca -t "C,C,C" -i "${tls_pki}/ca.crt"
    node /opt/noise-browser/server.mjs >/tmp/server.log 2>&1 &
    server_pid=$!
    trap '\''kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; rm -rf "$tls_pki"'\'' EXIT
    export TLS_KEY="${tls_pki}/server.key" TLS_CERT="${tls_pki}/server.crt"
    node - <<'\''NODE'\''
const fs = require("fs");
const https = require("https");
const { chromium } = require("/opt/noise-browser/node_modules/playwright");
const versions = require("/opt/noise-browser/browser-versions.json");
if (require("/opt/noise-browser/node_modules/playwright/package.json").version !== versions.playwright)
  throw new Error("Playwright client differs from the qualified version");

async function connect() {
  let lastError;
  for (let attempt = 0; attempt < 50; attempt++) {
    try {
      return await chromium.connect("ws://127.0.0.1:3000/");
    } catch (error) {
      lastError = error;
      await new Promise(resolve => setTimeout(resolve, 100));
    }
  }
  throw lastError;
}

(async () => {
  const tlsServer = https.createServer({
    key: fs.readFileSync(process.env.TLS_KEY),
    cert: fs.readFileSync(process.env.TLS_CERT),
  }, (_, response) => {
    response.writeHead(200, { "Content-Type": "text/html" });
    response.end("<title>Deck TLS acceptance</title>");
  });
  await new Promise(resolve => tlsServer.listen(8443, "127.0.0.1", resolve));
  try {
    const browser = await connect();
    if (browser.version() !== versions.chromium)
      throw new Error(`Unexpected running Chromium: ${browser.version()}`);
    console.log(JSON.stringify({ playwright: versions.playwright, chromium: browser.version() }));
    const page = await browser.newPage();
    await page.setContent("<title>Noise browser acceptance</title>");
    if (await page.title() !== "Noise browser acceptance")
      throw new Error("Chromium did not render the acceptance page");
    for (const host of ["dv-g3b-emudhra.we-have-a-problem.com", "dv-g3b-aaa-emudhra.we-have-a-problem.com"]) {
      const tlsPage = await browser.newPage();
      try {
        await tlsPage.goto(`https://${host}`, { timeout: 30000 });
        throw new Error(`Chromium accepted the compromised certificate chain at ${host}`);
      } catch (error) {
        if (!/net::ERR_CERT_(AUTHORITY_INVALID|REVOKED)/.test(String(error))) throw error;
      } finally {
        await tlsPage.close();
      }
    }
    const controlPage = await browser.newPage();
    const response = await controlPage.goto("https://localhost:8443", { timeout: 30000 });
    const title = await controlPage.title();
    if (response?.status() !== 200 || title !== "Deck TLS acceptance") {
      throw new Error(`Trusted HTTPS control returned an unexpected document: ${JSON.stringify({ status: response?.status() ?? null, url: controlPage.url(), title })}`);
    }
    await browser.close();
  } finally {
    await new Promise(resolve => tlsServer.close(resolve));
  }
})().catch(error => {
  console.error(error);
  process.exit(1);
});
NODE
  '
