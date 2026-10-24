"""FlowSource: picking each call's flow from the config service, with fallbacks.

A small fake config service (aiohttp) stands in for the Go service here; the
real one is exercised end to end in config-service's own tests and by hand."""

import asyncio
import json
import logging
from pathlib import Path

import aiohttp
import pytest
from aiohttp import web

from flow_engine import load_flow
from flow_engine.source import FROM_CACHE, FROM_FILE, FROM_SERVICE, FlowSource

ROOT = Path(__file__).resolve().parent.parent
NUMBER = "+918045678901"
TOKEN = "runtime-token"


def spec(path: str) -> dict:
    return json.loads((ROOT / path).read_text(encoding="utf-8"))


FALLBACK = load_flow(ROOT / "flows/complaint-helpline.json")


class FakeConfigService:
    """Answers /internal/v1/resolve like the Go service: ETag, 304, 404."""

    def __init__(self) -> None:
        self.mapped: dict | None = {"version": 1, "spec": spec("flows/emi-payment-issue.json")}
        self.requests: list[dict] = []
        self.status_override: int | None = None
        self.delay = 0.0

    async def resolve(self, request: web.Request) -> web.Response:
        self.requests.append(
            {
                "number": request.query.get("number"),
                "auth": request.headers.get("Authorization"),
                "if_none_match": request.headers.get("If-None-Match"),
            }
        )
        if self.delay:
            await asyncio.sleep(self.delay)
        if self.status_override:
            return web.Response(status=self.status_override)
        if request.headers.get("Authorization") != f"Bearer {TOKEN}":
            return web.json_response({"error": "missing or wrong token"}, status=401)
        if not self.mapped or request.query.get("number") != NUMBER:
            return web.json_response({"error": "not mapped"}, status=404)
        etag = f'"{self.mapped["spec"]["flow_id"]}.v{self.mapped["version"]}"'
        if request.headers.get("If-None-Match") == etag:
            return web.Response(status=304, headers={"ETag": etag})
        body = {"number": NUMBER, "flow_id": self.mapped["spec"]["flow_id"], **self.mapped, "checksum": "x"}
        return web.json_response(body, headers={"ETag": etag})


@pytest.fixture
async def service():
    fake = FakeConfigService()
    app = web.Application()
    app.router.add_get("/internal/v1/resolve", fake.resolve)
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "127.0.0.1", 0)
    await site.start()
    port = site._server.sockets[0].getsockname()[1]
    fake.url = f"http://127.0.0.1:{port}"
    yield fake
    await runner.cleanup()


@pytest.fixture
async def http():
    async with aiohttp.ClientSession() as session:
        yield session


def source(service, tmp_path, **kw) -> FlowSource:
    return FlowSource(fallback=FALLBACK, url=service.url, token=TOKEN, cache_dir=tmp_path, **kw)


async def test_without_config_service_every_call_uses_the_file(http):
    got = await FlowSource(fallback=FALLBACK).flow_for(NUMBER, http)
    assert (got.flow, got.source, got.version) == (FALLBACK, FROM_FILE, None)


def test_url_without_token_is_refused():
    with pytest.raises(RuntimeError, match="CONFIG_SERVICE_TOKEN"):
        FlowSource(fallback=FALLBACK, url="http://config-service:8080")


async def test_flow_comes_from_the_service_then_304_reuses_it(service, http, tmp_path):
    src = source(service, tmp_path)
    first = await src.flow_for(NUMBER, http)
    assert (first.flow.flow_id, first.source, first.version) == ("emi_payment_issue", FROM_SERVICE, 1)
    assert service.requests[0] == {"number": NUMBER, "auth": f"Bearer {TOKEN}", "if_none_match": None}

    second = await src.flow_for(NUMBER, http)
    assert service.requests[1]["if_none_match"] == '"emi_payment_issue.v1"'
    assert second.flow is first.flow and second.source == FROM_SERVICE  # 304: not parsed again


async def test_a_published_change_applies_on_the_next_call(service, http, tmp_path):
    src = source(service, tmp_path)
    await src.flow_for(NUMBER, http)
    changed = spec("flows/emi-payment-issue.json")
    changed["greeting"] = "Namaste, naya greeting."
    service.mapped = {"version": 2, "spec": changed}

    got = await src.flow_for(NUMBER, http)
    assert (got.version, got.flow.greeting) == (2, "Namaste, naya greeting.")


async def test_unmapped_number_uses_the_file_and_forgets_the_cache(service, http, tmp_path):
    src = source(service, tmp_path)
    await src.flow_for(NUMBER, http)
    assert list(tmp_path.glob("*.json"))
    service.mapped = None

    got = await src.flow_for(NUMBER, http)
    assert (got.flow, got.source) == (FALLBACK, FROM_FILE)
    assert not list(tmp_path.glob("*.json"))


async def test_service_down_uses_the_last_flow_it_gave(service, http, tmp_path, caplog):
    src = source(service, tmp_path)
    await src.flow_for(NUMBER, http)
    service.status_override = 503
    got = await src.flow_for(NUMBER, http)
    assert (got.flow.flow_id, got.source, got.version) == ("emi_payment_issue", FROM_CACHE, 1)

    src.url = "http://127.0.0.1:9"  # nothing listens here
    with caplog.at_level(logging.ERROR, logger="flow-source"):
        got = await src.flow_for(NUMBER, http)
    assert got.source == FROM_CACHE
    assert "not reachable" in caplog.text


async def test_a_new_process_reads_the_disk_cache(service, http, tmp_path):
    await source(service, tmp_path).flow_for(NUMBER, http)
    fresh = source(service, tmp_path)  # e.g. a new job process
    service.status_override = 500
    got = await fresh.flow_for(NUMBER, http)
    assert (got.flow.flow_id, got.source) == ("emi_payment_issue", FROM_CACHE)
    # The disk cache also lets it revalidate with the ETag instead of downloading again.
    assert service.requests[-1]["if_none_match"] == '"emi_payment_issue.v1"'


async def test_slow_service_falls_back_within_the_timeout(service, http, tmp_path):
    service.delay = 2
    src = source(service, tmp_path, timeout=0.3)
    loop = asyncio.get_running_loop()
    start = loop.time()
    got = await src.flow_for(NUMBER, http)
    assert got.source == FROM_FILE
    assert loop.time() - start < 1.0


async def test_a_flow_the_agent_cant_run_is_not_used(service, http, tmp_path, caplog):
    src = source(service, tmp_path)
    await src.flow_for(NUMBER, http)
    broken = spec("flows/emi-payment-issue.json")
    broken["start_node"] = "nowhere"
    service.mapped = {"version": 2, "spec": broken}

    with caplog.at_level(logging.ERROR, logger="flow-source"):
        got = await src.flow_for(NUMBER, http)
    assert (got.source, got.version) == (FROM_CACHE, 1)
    assert "'nowhere' is not a node" in caplog.text


async def test_wrong_token_falls_back(service, http, tmp_path):
    src = FlowSource(fallback=FALLBACK, url=service.url, token="wrong", cache_dir=tmp_path)
    got = await src.flow_for(NUMBER, http)
    assert got.source == FROM_FILE


async def test_no_dialled_number_uses_the_file(service, http, tmp_path):
    got = await source(service, tmp_path).flow_for("", http)
    assert got.source == FROM_FILE and not service.requests
