"""Verify the Krill identity token, with the standard library only.

Why pure Python: F6 leaves app guests with no outbound network at all, and the
build allowlist may not include PyPI, so an app that needs `pip install
pynacl` is an app that may not build. Ed25519 verification is ~60 lines of
RFC 8032 arithmetic; that is a better trade than a dependency for a platform
whose whole pitch is "ship a plain Dockerfile".

The three checks are not optional, and each maps to a gate:

    signature  F1 — "a token the app can verify, not a bare header"
    audience   F3 — the token names exactly one app
    expiry     F3 — a leaked token dies in minutes

The public key arrives as KRILL_IDENTITY_PUBKEY, which krilld puts on the
guest's kernel command line and the generated init exports.
"""

import base64
import json
import os
import time

# Ed25519 parameters (RFC 8032, section 5.1).
_P = 2 ** 255 - 19
_L = 2 ** 252 + 27742317777372353535851937790883648493
_D = -121665 * pow(121666, _P - 2, _P) % _P
_I = pow(2, (_P - 1) // 4, _P)


def _recover_x(y, sign):
    xx = (y * y - 1) * pow(_D * y * y + 1, _P - 2, _P)
    x = pow(xx, (_P + 3) // 8, _P)
    if (x * x - xx) % _P != 0:
        x = (x * _I) % _P
    if (x * x - xx) % _P != 0:
        return None
    if x % 2 != sign:
        x = _P - x
    return x


def _add(p, q):
    x1, y1, z1, t1 = p
    x2, y2, z2, t2 = q
    a = (y1 - x1) * (y2 - x2) % _P
    b = (y1 + x1) * (y2 + x2) % _P
    c = t1 * 2 * _D * t2 % _P
    dd = z1 * 2 * z2 % _P
    e, f, g, h = b - a, dd - c, dd + c, b + a
    return (e * f % _P, g * h % _P, f * g % _P, e * h % _P)


def _mul(p, s):
    q = (0, 1, 1, 0)
    while s > 0:
        if s & 1:
            q = _add(q, p)
        p = _add(p, p)
        s >>= 1
    return q


_By = 4 * pow(5, _P - 2, _P) % _P
_Bx = _recover_x(_By, 0)
_B = (_Bx, _By, 1, _Bx * _By % _P)


def _decode_point(raw):
    if len(raw) != 32:
        return None
    n = int.from_bytes(raw, "little")
    sign, y = n >> 255, n & ((1 << 255) - 1)
    if y >= _P:
        return None
    x = _recover_x(y, sign)
    if x is None:
        return None
    return (x, y, 1, x * y % _P)


def _equal(p, q):
    x1, y1, z1, _ = p
    x2, y2, z2, _ = q
    return (x1 * z2 - x2 * z1) % _P == 0 and (y1 * z2 - y2 * z1) % _P == 0


def ed25519_verify(public_key: bytes, message: bytes, signature: bytes) -> bool:
    """RFC 8032 Ed25519 verification. Returns False rather than raising."""
    import hashlib

    if len(signature) != 64:
        return False
    a = _decode_point(public_key)
    if a is None:
        return False
    r = _decode_point(signature[:32])
    if r is None:
        return False
    s = int.from_bytes(signature[32:], "little")
    if s >= _L:
        return False
    h = int.from_bytes(
        hashlib.sha512(signature[:32] + public_key + message).digest(), "little"
    ) % _L
    return _equal(_mul(_B, s), _add(r, _mul(a, h)))


def _b64(s: str) -> bytes:
    return base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))


class Unverified(Exception):
    """The caller could not be established. Never fall back to the header."""


def verify(token: str, audience: str, public_key_b64: str | None = None,
           now: float | None = None) -> dict:
    """Return the token's claims, or raise Unverified."""
    key_b64 = public_key_b64 or os.environ.get("KRILL_IDENTITY_PUBKEY", "")
    if not key_b64:
        raise Unverified("no KRILL_IDENTITY_PUBKEY: this guest was booted without one")
    if not token:
        raise Unverified("no token")
    parts = token.split(".")
    if len(parts) != 3:
        raise Unverified("malformed token")
    header = json.loads(_b64(parts[0]))
    # Pin the algorithm to what we hold a key for. Reading "alg" from an
    # attacker-supplied header and believing it is the oldest JWT bug there is.
    if header.get("alg") != "EdDSA":
        raise Unverified(f"unsupported algorithm {header.get('alg')!r}")
    if not ed25519_verify(_b64(key_b64), f"{parts[0]}.{parts[1]}".encode(), _b64(parts[2])):
        raise Unverified("bad signature")
    claims = json.loads(_b64(parts[1]))
    if claims.get("aud") != audience:
        raise Unverified(f"token audience {claims.get('aud')!r} is not {audience!r}")
    if (now or time.time()) >= claims.get("exp", 0):
        raise Unverified("token expired")
    return claims
