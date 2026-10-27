"""Remove the distrusted eMudhra G3 root from Python's system and certifi trust."""
import hashlib
from pathlib import Path
import re
import ssl

import certifi


bundle = Path(ssl.get_default_verify_paths().cafile).resolve()
certificates = re.findall(rb"-----BEGIN CERTIFICATE-----.*?-----END CERTIFICATE-----", bundle.read_bytes(), re.DOTALL)
if not certificates:
    raise RuntimeError("System CA bundle is empty")
# This root signed the compromised eMudhra intermediates.
distrusted = "86a1ecba089c4a8d3bbe2734c612ba341d813e043cf9e8a862cd5c57a36bbe6b"
trusted = [pem for pem in certificates if hashlib.sha256(ssl.PEM_cert_to_DER_cert(pem.decode())).hexdigest() != distrusted]
if not trusted:
    raise RuntimeError("Removing the distrusted root would empty the CA bundle")
bundle.write_bytes(b"\n".join(trusted) + b"\n")
certifi_bundle = Path(certifi.where())
if certifi_bundle.resolve() != bundle:
    certifi_bundle.unlink()
    certifi_bundle.symlink_to(bundle)
for context in (ssl.create_default_context(), ssl.create_default_context(cafile=certifi.where())):
    roots = context.get_ca_certs()
    if not roots or any(value == "emSign ECC Root CA - G3" for cert in roots for rdn in cert["subject"] for key, value in rdn):
        raise RuntimeError("Python trust store retains the distrusted root or has no CAs")
