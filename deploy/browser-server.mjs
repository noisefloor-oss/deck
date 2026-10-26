import { chromium } from "playwright";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const versions = JSON.parse(readFileSync(new URL("./browser-versions.json", import.meta.url), "utf8"));
if (require("playwright/package.json").version !== versions.playwright) {
  throw new Error("Browser service Playwright package differs from the qualified version");
}

const endpoint = "ws://127.0.0.1:3000/";
const server = await chromium.launchServer({
  executablePath: "/opt/noise-browser/chrome/chrome",
  headless: true,
  host: "127.0.0.1",
  port: 3000,
  wsPath: "/",
});

const check = await chromium.connect(server.wsEndpoint());
try {
  if (check.version() !== versions.chromium) {
    await server.close();
    throw new Error(`Browser version differs from the qualified version: ${check.version()}`);
  }
} finally {
  await check.close();
}

if (server.wsEndpoint() !== endpoint) {
  await server.close();
  throw new Error(`unexpected browser endpoint: ${server.wsEndpoint()}`);
}

let closing = false;
async function shutdown() {
  if (closing) return;
  closing = true;
  await server.close();
}

for (const signal of ["SIGINT", "SIGTERM"]) {
  process.once(signal, () => {
    shutdown().catch((error) => {
      console.error(error);
      process.exitCode = 1;
    });
  });
}
