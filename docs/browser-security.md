# Browser security updates

The browser image pairs Playwright 1.64.1 with Chrome for Testing
156.0.8078.47. Playwright's 1.64.1 bundle contains Chromium 156.0.8078.4,
which predates the [October 20 security release](https://chromereleases.googleblog.com/2026/10/stable-channel-update-for-desktop_02018666648.html).
Matching the Playwright client and service versions alone does not establish
that Chromium contains those fixes.

`deploy/browser-versions.json` records the pair expected by the service and
its acceptance probe. The Dockerfile pins the official Linux amd64 and arm64
Chrome for Testing archives by SHA-256, removes the older bundled Chromium,
and installs the replacement at a fixed executable path. The browser service
uses that path explicitly and refuses a different Playwright or Chromium
version. The Compose image tag names both versions. Downloads happen during
the image build, never at service startup.

For each update, check the current Chrome security advisory and the official
[Chrome for Testing version inventory](https://googlechromelabs.github.io/chrome-for-testing/),
then qualify the pair with the real sidecar acceptance test. A matching major
version is not proof of compatibility. Preserve the certificate distrust
checks and the trusted HTTPS control in that test.

The release acceptance workflow builds the sidecar, exercises it, and scans
the exact built image ID with checksum-pinned Trivy. High or critical findings
fail acceptance, including findings without a known fix. The JSON report is
retained even on failure. An unavailable scanner or vulnerability database
also blocks acceptance. The image scan complements the browser version check;
an operating-system package inventory alone does not prove the downloaded
Chrome binary contains the required fixes.

Deploy through your installation's maintained rollout workflow. Rebuilding the app alone
does not replace the browser service. Inventory the affected instances before
rollout, retain the previous image digests for recovery, and run the maintained
verification on each instance after rollout. It compares the app's Playwright
client to the service pin, checks the connected browser version, and renders
the login page. Record the source revision, image digest, scan report, observed
versions and browser smoke result for each instance. Do not mark rollout
complete while an affected instance remains unverified.
