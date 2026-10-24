"""Personal data must never reach the logs (log_privacy.py)."""

import io
import json
import logging
import pickle

import pytest

import log_privacy
from log_privacy import REDACTED, mask_phone_numbers


@pytest.fixture
def captured():
    """A logger writing JSON lines through LiveKit's own production formatter."""
    from livekit.agents.cli.log import JsonFormatter

    log_privacy.install()
    stream = io.StringIO()
    handler = logging.StreamHandler(stream)
    handler.setFormatter(JsonFormatter())
    logger = logging.getLogger("test.privacy")
    logger.handlers, logger.propagate = [handler], False
    logger.setLevel(logging.INFO)
    yield logger, stream
    logger.handlers = []


def lines(stream) -> list[dict]:
    return [json.loads(line) for line in stream.getvalue().splitlines() if line.strip()]


def test_phone_numbers_keep_only_country_code_and_last_4():
    assert mask_phone_numbers("+919990187773") == "+91******7773"
    assert mask_phone_numbers("9990187773") == "******7773"
    assert mask_phone_numbers("room +919990187773_gZ3wWh3NfnFk") == "room +91******7773_gZ3wWh3NfnFk"
    assert mask_phone_numbers("sip_+919990187773") == "sip_+91******7773"
    # Short numbers (ports, counts, durations) are left alone.
    assert mask_phone_numbers("port 8081, 900 s, AJ_rKDCYj85SYVt") == "port 8081, 900 s, AJ_rKDCYj85SYVt"


def test_livekit_style_log_line_is_masked(captured):
    logger, stream = captured
    # The shape of a real line from your EC2 logs.
    logger.info(
        "closing agent session due to participant disconnect",
        extra={
            "room": "+919990187773_gZ3wWh3NfnFk",
            "participant": "sip_+919990187773",
            "lk.pii.participant_identity": "sip_+919990187773",
            "lk.pii.dtmf_inputs": "4 3 2 1",
            "job_id": "AJ_rKDCYj85SYVt",
        },
    )
    out = lines(stream)[0]
    text = json.dumps(out)
    assert "9990187773" not in text and "4 3 2 1" not in text
    assert out["room"] == "+91******7773_gZ3wWh3NfnFk"
    assert out["participant"] == "sip_+91******7773"
    assert out["lk.pii.dtmf_inputs"] == REDACTED
    assert out["lk.pii.participant_identity"] == REDACTED
    assert out["job_id"] == "AJ_rKDCYj85SYVt"  # non-personal fields untouched


def test_message_text_and_args_are_masked(captured):
    logger, stream = captured
    logger.info("calling %s now", "+919990187773")
    logger.info("dialled 9990187773 directly")
    msgs = [line["message"] for line in lines(stream)]
    assert msgs == ["calling +91******7773 now", "dialled ******7773 directly"]


def test_lines_forwarded_from_a_call_process_are_masked(captured):
    """Each call runs in its own process; LiveKit pickles its log lines and the main
    process writes them via callHandlers(). They must be masked there too."""
    logger, stream = captured
    record = logging.LogRecord("test.privacy", logging.INFO, __file__, 1, "RoomIO linked", None, None)
    record.__dict__["room"] = "+919990187773_QyFunp5Lgg3t"
    record.__dict__["lk.pii.participant_identity"] = "sip_+919990187773"
    forwarded = pickle.loads(pickle.dumps(record))  # what crosses the process boundary
    logger.callHandlers(forwarded)
    out = lines(stream)[0]
    assert out["room"] == "+91******7773_QyFunp5Lgg3t"
    assert out["lk.pii.participant_identity"] == REDACTED


def test_flow_engine_lines_are_unchanged(captured):
    logger, stream = captured
    logger.info("flow field asked", extra={"node": "collect_details", "field": "full_name"})
    out = lines(stream)[0]
    assert (out["message"], out["node"], out["field"]) == ("flow field asked", "collect_details", "full_name")
