"""Read exactly one pinned private Soodles oracle; no anonymous fallback.

The original GitHub Actions owner supplies the read-only cross-repository
credential. This fetcher never grants Noodle or Soodles execution authority.
"""
from __future__ import annotations

import base64
import hashlib
import json
import urllib.request

COMMIT = "0256f2923e978b989e25df07c74db4370d343312"
BLOB = "7793fb46b8a297fafd478b7fc812da1aba7e7b2e"
SHA256 = "d65b8ba15f2cdbfdd43c4fc0bf267c78ccb2b38208171623b1291704b144c6d9"
API_URL = (
    "https://api.github.com/repos/ed3c/soodles/contents/soodles.py?ref=" + COMMIT
)


class OracleAccessError(ValueError):
    pass


def fetch_fixed_oracle(token: str | None, *, urlopen=urllib.request.urlopen) -> bytes:
    if not token:
        raise OracleAccessError("SOODLES_ORACLE_READ_TOKEN_REQUIRED")
    request = urllib.request.Request(API_URL, headers={
        "Authorization": "Bearer " + token,
        "Accept": "application/vnd.github+json",
        "User-Agent": "noodle-pinned-oracle-reader",
        "X-GitHub-Api-Version": "2022-11-28",
    })
    try:
        with urlopen(request, timeout=30) as response:
            manifest = json.loads(response.read())
    except urllib.error.HTTPError as error:
        if error.code in (401, 403, 404):
            raise OracleAccessError("SOODLES_ORACLE_ACCESS_DENIED_OR_MISSING") from None
        raise
    except (json.JSONDecodeError, UnicodeDecodeError) as error:
        raise OracleAccessError("SOODLES_ORACLE_RESPONSE_INVALID") from error
    if (not isinstance(manifest, dict) or manifest.get("type") != "file"
            or manifest.get("sha") != BLOB
            or manifest.get("encoding") != "base64"
            or not isinstance(manifest.get("content"), str)):
        raise OracleAccessError("SOODLES_ORACLE_SOURCE_IDENTITY_CHANGED")
    try:
        content = base64.b64decode("".join(manifest["content"].split()), validate=True)
    except (ValueError, base64.binascii.Error) as error:
        raise OracleAccessError("SOODLES_ORACLE_ENCODING_INVALID") from error
    if hashlib.sha256(content).hexdigest() != SHA256:
        raise OracleAccessError("SOODLES_ORACLE_RAW_DIGEST_CHANGED")
    return content
