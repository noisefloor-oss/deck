"""Exercise trust-store patching against temporary CA bundles only."""
import hashlib
import os
from pathlib import Path
import re
import runpy
import ssl
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parent
BAD = '86a1ecba089c4a8d3bbe2734c612ba341d813e043cf9e8a862cd5c57a36bbe6b'


def fingerprint(pem):
    return hashlib.sha256(ssl.PEM_cert_to_DER_cert(pem.decode())).hexdigest()


class TrustStorePatchTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cafile = ssl.get_default_verify_paths().cafile
        if not cafile or not Path(cafile).is_file():
            raise unittest.SkipTest('requires a PEM system CA bundle')
        bundle = Path(cafile).read_bytes()
        certificates = re.findall(rb'-----BEGIN CERTIFICATE-----.*?-----END CERTIFICATE-----', bundle, re.DOTALL)
        cls.good = next(pem for pem in certificates if fingerprint(pem) != BAD)
        cls.bad = (ROOT.parent / 'distrusted-emudhra-g3.crt').read_bytes().strip()
        assert fingerprint(cls.bad) == BAD

    def apply(self, bundle, certifi):
        module = types.SimpleNamespace(where=lambda: str(certifi))
        with patch.dict(sys.modules, {'certifi': module}), patch.dict(os.environ, {'SSL_CERT_FILE': str(bundle)}):
            runpy.run_path(str(ROOT / 'patch-python-tls-roots.py'), run_name='__main__')

    def test_removes_root_from_both_consumers_and_preserves_other_cas(self):
        with tempfile.TemporaryDirectory() as directory:
            bundle, certifi = Path(directory) / 'system.pem', Path(directory) / 'certifi.pem'
            bundle.write_bytes(self.good + b'\n' + self.bad + b'\n')
            certifi.write_bytes(self.bad + b'\n')
            self.apply(bundle, certifi)
            self.assertEqual(bundle.read_bytes(), self.good + b'\n')
            self.assertTrue(certifi.is_symlink())
            self.assertEqual(certifi.resolve(), bundle)
            self.apply(bundle, certifi)
            self.assertEqual(bundle.read_bytes(), self.good + b'\n')

    def test_empty_bundle_refused_without_changing_certifi(self):
        with tempfile.TemporaryDirectory() as directory:
            bundle, certifi = Path(directory) / 'system.pem', Path(directory) / 'certifi.pem'
            bundle.write_bytes(b'')
            certifi.write_bytes(self.good)
            with self.assertRaisesRegex(RuntimeError, 'empty'):
                self.apply(bundle, certifi)
            self.assertEqual(certifi.read_bytes(), self.good)

    def test_only_distrusted_root_refused_without_emptying_bundle(self):
        with tempfile.TemporaryDirectory() as directory:
            bundle, certifi = Path(directory) / 'system.pem', Path(directory) / 'certifi.pem'
            bundle.write_bytes(self.bad)
            certifi.write_bytes(self.good)
            with self.assertRaisesRegex(RuntimeError, 'empty'):
                self.apply(bundle, certifi)
            self.assertEqual(bundle.read_bytes(), self.bad)


if __name__ == '__main__':
    unittest.main()
