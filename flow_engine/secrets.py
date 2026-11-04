"""Opening integration credentials sealed by the config service.

A developer types an integration's token or client secret into Flow Studio;
the config service seals it at once with this agent's public key (see
config-service/internal/secrets) and stores only that. The private key,
INTEGRATION_SECRETS_KEY in the agent's .env, never leaves this container, so
neither the database nor the config service can read a credential: only the
agent, during a call.

Make a key pair once:
    python -m flow_engine.secrets keygen
Put the private key in the agent's .env and the public key in the platform's
infra/.env (INTEGRATION_SECRETS_PUBLIC_KEY).
"""

from __future__ import annotations

import base64
import binascii
import os
import sys

from cryptography.exceptions import InvalidTag
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey, X25519PublicKey
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.kdf.hkdf import HKDF
from cryptography.hazmat.primitives.serialization import Encoding, PrivateFormat, PublicFormat, NoEncryption

PREFIX = "v1."
INFO = b"flow-studio integration secret v1"


class SecretUnavailable(Exception):
    """The credential can't be opened (no key, wrong key, or damaged)."""


def _raw_public(key: X25519PublicKey) -> bytes:
    return key.public_bytes(Encoding.Raw, PublicFormat.Raw)


def private_key(b64: str | None) -> X25519PrivateKey | None:
    if not b64:
        return None
    try:
        raw = base64.b64decode(b64.strip())
    except (binascii.Error, ValueError):
        return None
    return X25519PrivateKey.from_private_bytes(raw) if len(raw) == 32 else None


def _key(shared: bytes, ephemeral: bytes, recipient: bytes) -> bytes:
    return HKDF(algorithm=hashes.SHA256(), length=32, salt=ephemeral + recipient, info=INFO).derive(shared)


def seal(public: X25519PublicKey, aad: str, plaintext: str) -> str:
    """Seal like the config service does (for tests and tooling)."""
    eph = X25519PrivateKey.generate()
    eph_pub = _raw_public(eph.public_key())
    k = _key(eph.exchange(public), eph_pub, _raw_public(public))
    nonce = os.urandom(12)
    return PREFIX + base64.b64encode(eph_pub + nonce + AESGCM(k).encrypt(nonce, plaintext.encode(), aad.encode())).decode()


def open_sealed(key: X25519PrivateKey | None, aad: str, sealed: str) -> str:
    """The credential in the clear. Raises SecretUnavailable."""
    if key is None:
        raise SecretUnavailable("INTEGRATION_SECRETS_KEY isn't set")
    if not sealed.startswith(PREFIX):
        raise SecretUnavailable("not a sealed secret")
    try:
        raw = base64.b64decode(sealed[len(PREFIX) :])
        eph_pub, nonce, body = raw[:32], raw[32:44], raw[44:]
        k = _key(key.exchange(X25519PublicKey.from_public_bytes(eph_pub)), eph_pub, _raw_public(key.public_key()))
        return AESGCM(k).decrypt(nonce, body, aad.encode()).decode()
    except (InvalidTag, ValueError, binascii.Error) as e:
        raise SecretUnavailable("the secret couldn't be opened (sealed for another key or integration?)") from e


def keygen() -> tuple[str, str]:
    """A new key pair: (private for the agent, public for the config service), base64."""
    key = X25519PrivateKey.generate()
    private = key.private_bytes(Encoding.Raw, PrivateFormat.Raw, NoEncryption())
    return base64.b64encode(private).decode(), base64.b64encode(_raw_public(key.public_key())).decode()


if __name__ == "__main__":
    if sys.argv[1:] != ["keygen"]:
        sys.exit("usage: python -m flow_engine.secrets keygen")
    priv, pub = keygen()
    print("# the agent's .env (keep it there only):")
    print(f"INTEGRATION_SECRETS_KEY={priv}")
    print("# the platform's infra/.env (for the config service):")
    print(f"INTEGRATION_SECRETS_PUBLIC_KEY={pub}")
