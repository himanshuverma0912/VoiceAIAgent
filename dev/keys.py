"""Makes the local setup's integration key pair (X25519) with only the Python
standard library, so `make dev` works on a fresh Mac without pip installs.

Same format as `python -m flow_engine.secrets keygen`: base64 of the raw 32-byte
private and public keys. Prints two lines for dev/.env.
"""

import base64
import os

P = 2**255 - 19
A24 = 121665


def _clamp(k: bytes) -> int:
    b = bytearray(k)
    b[0] &= 248
    b[31] &= 127
    b[31] |= 64
    return int.from_bytes(b, "little")


def x25519(scalar: bytes, u: int = 9) -> bytes:
    """RFC 7748 scalar multiplication (the Montgomery ladder)."""
    k = _clamp(scalar)
    x1, x2, z2, x3, z3, swap = u, 1, 0, u, 1, 0
    for t in reversed(range(255)):
        bit = (k >> t) & 1
        swap ^= bit
        if swap:
            x2, x3, z2, z3 = x3, x2, z3, z2
        swap = bit
        a, b = (x2 + z2) % P, (x2 - z2) % P
        aa, bb = a * a % P, b * b % P
        e = (aa - bb) % P
        c, d = (x3 + z3) % P, (x3 - z3) % P
        da, cb = d * a % P, c * b % P
        x3, z3 = (da + cb) ** 2 % P, x1 * (da - cb) ** 2 % P
        x2, z2 = aa * bb % P, e * (aa + A24 * e) % P
    if swap:
        x2, z2 = x3, z3
    return (x2 * pow(z2, P - 2, P) % P).to_bytes(32, "little")


if __name__ == "__main__":
    private = os.urandom(32)
    print("INTEGRATION_SECRETS_KEY=" + base64.b64encode(private).decode())
    print("INTEGRATION_SECRETS_PUBLIC_KEY=" + base64.b64encode(x25519(private)).decode())
