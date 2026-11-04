"""Creates dev/.env for the local setup, or fills in what's missing from it.

Run by `make dev` before anything starts. Only the Python standard library is
used (macOS has python3 with Xcode's command line tools). Values already in
dev/.env, like your own API keys, are never changed.
"""

import base64
import re
import secrets
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import keys  # noqa: E402

ENV = HERE / ".env"
EXAMPLE = HERE / ".env.example"


def hex_(n: int):
    return lambda: secrets.token_hex(n)


MADE = {
    "DEV_USER_PASSWORD": hex_(8),  # 16 characters (the directory wants at least 12)
    "KEYCLOAK_ADMIN_PASSWORD": hex_(16),
    "PLATFORM_DB_PASSWORD": hex_(16),
    "CONFIG_ADMIN_TOKEN": hex_(24),
    "CONFIG_RUNTIME_TOKEN": hex_(24),
    "LITELLM_MASTER_KEY": lambda: "sk-" + secrets.token_hex(24),
    "LIVEKIT_API_SECRET": hex_(24),
    "RESUME_KEY": lambda: base64.b64encode(secrets.token_bytes(32)).decode(),
    "DEMO_BANK_TOKEN": hex_(24),
    "EMI_CORE_TOKEN": hex_(24),
    "EMI_HELPDESK_TOKEN": hex_(24),
}


def main() -> None:
    created = not ENV.exists()
    text = ENV.read_text() if not created else EXAMPLE.read_text()
    values = dict(re.findall(r"^([A-Z0-9_]+)=(.*)$", text, re.M))

    fill: dict[str, str] = {}
    for name, make in MADE.items():
        if not values.get(name, "").strip():
            fill[name] = make()
    if not values.get("INTEGRATION_SECRETS_KEY", "").strip() or not values.get("INTEGRATION_SECRETS_PUBLIC_KEY", "").strip():
        private = secrets.token_bytes(32)
        fill["INTEGRATION_SECRETS_KEY"] = base64.b64encode(private).decode()
        fill["INTEGRATION_SECRETS_PUBLIC_KEY"] = base64.b64encode(keys.x25519(private)).decode()

    for name, value in fill.items():
        if re.search(rf"^{name}=", text, re.M):
            text = re.sub(rf"^{name}=.*$", f"{name}={value}", text, count=1, flags=re.M)
        else:
            text += f"\n{name}={value}\n"
    if created or fill:
        ENV.write_text(text)
        ENV.chmod(0o600)
    if created:
        print("Created dev/.env with random local secrets. Add your own API keys there for spoken test calls.")
    elif fill:
        print(f"dev/.env: filled in {', '.join(sorted(fill))}")


if __name__ == "__main__":
    main()
