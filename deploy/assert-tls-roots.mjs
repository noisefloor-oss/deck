import assert from "node:assert/strict";
import { X509Certificate } from "node:crypto";
import { getCACertificates } from "node:tls";

for (const source of ["bundled", "system"]) {
  const roots = getCACertificates(source);
  assert(roots.length > 0, `${source} CA store is empty`);
  assert(!roots.some(pem => new X509Certificate(pem).subject.split("\n")
    .includes("CN=emSign ECC Root CA - G3")), `${source} CA store trusts eMudhra G3`);
}
