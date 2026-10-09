"""Offline failure controls; never contact the private Soodles repository."""
import base64
import hashlib
import json
import unittest
import urllib.error
from unittest.mock import patch

from scripts import fixed_soodles_oracle as oracle


class Response:
    def __init__(self, value):
        self.value = value

    def __enter__(self):
        return self

    def __exit__(self, *_):
        return None

    def read(self):
        return json.dumps(self.value).encode("utf-8")


class OracleAccessTests(unittest.TestCase):
    def metadata(self, content=b"synthetic exact oracle"):
        return {
            "type": "file",
            "sha": oracle.BLOB,
            "encoding": "base64",
            "content": base64.b64encode(content).decode("ascii"),
        }

    def test_exact_pins_and_authenticated_source_bytes(self):
        raw = b"synthetic exact oracle"
        checks = []
        def urlopen(request, timeout):
            checks.append((request.full_url, request.get_header("Authorization"), timeout))
            return Response(self.metadata(raw))
        with patch.object(oracle, "SHA256", hashlib.sha256(raw).hexdigest()):
            self.assertEqual(oracle.fetch_fixed_oracle("test-token", urlopen=urlopen), raw)
        self.assertEqual(checks, [(oracle.API_URL, "Bearer test-token", 30)])
        self.assertIn("0256f2923e978b989e25df07c74db4370d343312", oracle.API_URL)

    def test_absent_token_refuses_before_network(self):
        def prohibited(*_args, **_kwargs):
            raise AssertionError("network must not be contacted")
        for token in (None, ""):
            with self.assertRaisesRegex(oracle.OracleAccessError, "SOODLES_ORACLE_READ_TOKEN_REQUIRED"):
                oracle.fetch_fixed_oracle(token, urlopen=prohibited)

    def test_cross_private_repository_denied_is_not_missing_source_proof(self):
        def no_access(request, timeout):
            raise urllib.error.HTTPError(request.full_url, 404, "Not Found", {}, None)
        with self.assertRaisesRegex(oracle.OracleAccessError, "SOODLES_ORACLE_ACCESS_DENIED_OR_MISSING"):
            oracle.fetch_fixed_oracle("bad-credential", urlopen=no_access)

    def test_wrong_raw_digest_refuses(self):
        with self.assertRaisesRegex(oracle.OracleAccessError, "SOODLES_ORACLE_RAW_DIGEST_CHANGED"):
            oracle.fetch_fixed_oracle("token", urlopen=lambda *_args, **_kwargs: Response(
                self.metadata(b"wrong original bytes")))

    def test_wrong_blob_or_json_shape_refuses(self):
        for changed in ({"sha": "0" * 40}, {"type": "dir"}, {"encoding": "raw"}):
            manifest = self.metadata()
            manifest.update(changed)
            with self.subTest(changed=changed), self.assertRaisesRegex(
                    oracle.OracleAccessError, "SOODLES_ORACLE_SOURCE_IDENTITY_CHANGED"):
                oracle.fetch_fixed_oracle(
                    "token", urlopen=lambda *_args, **_kwargs: Response(manifest))

    def test_invalid_base64_refuses(self):
        manifest = self.metadata()
        manifest["content"] = "not base64 @@@@@"
        with self.assertRaisesRegex(oracle.OracleAccessError, "SOODLES_ORACLE_ENCODING_INVALID"):
            oracle.fetch_fixed_oracle(
                "token", urlopen=lambda *_args, **_kwargs: Response(manifest))


if __name__ == "__main__":
    unittest.main()
