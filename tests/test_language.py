"""Choosing the reply language from what the caller just said (flow_engine/language.py)."""

import pytest
from livekit.agents import llm

from flow_engine.language import ENGLISH, HINDI, HINTS, MIXED, LanguageHint, detect


@pytest.mark.parametrize(
    "text, expected",
    [
        # From real test calls where the agent kept answering in Hindi.
        ("It's thirtieth September nineteen ninety six.", ENGLISH),
        ("It's fourteen august nineteen ninety.", ENGLISH),
        ("Hello, there has been no water supply in my area for three days.", ENGLISH),
        ("yeah, my name is हिमांशु and my mobile number is one two three", ENGLISH),  # a name in Hindi script
        ("मेरे इलाके में तीन दिन से बिजली नहीं है।", HINDI),
        ("मेरी EMI पाँच अक्टूबर को दो बार कट गई।", HINDI),  # English loanword
        ("Mere area mein road pe bahut bada gaddha hai", MIXED),  # Hinglish in Roman letters
        ("मेरा loan account का EMI bounce हो गया this month", MIXED),
        # Too short to tell: keep whatever language was in use.
        ("Ok ok.", None),
        ("Yeah continue.", None),
        ("हाँ", None),
        ("", None),
    ],
)
def test_detect(text, expected):
    assert detect(text) == expected


def ctx(*turns):
    c = llm.ChatContext()
    c.add_message(role="system", content="persona and step instructions")
    for role, text in turns:
        c.add_message(role=role, content=text)
    return c


def last_system(c: llm.ChatContext) -> str | None:
    last = c.items[-1]
    return last.text_content if last.role == "system" else None


def test_english_reply_after_english_sentence_despite_hindi_so_far():
    hint = LanguageHint("hi")
    sent = hint.apply(
        ctx(
            ("assistant", "नमस्ते, Demo Finance loan helpline में आपका स्वागत है।"),
            ("user", "Ok ok."),
            ("assistant", "आपकी पहचान के लिए, क्या आप मुझे अपना जन्मतिथि बता सकती हैं?"),
            ("user", "It's fourteen august nineteen ninety."),
        )
    )
    assert last_system(sent) == HINTS[ENGLISH]


def test_short_replies_keep_the_language_in_use():
    hint = LanguageHint("hi")
    assert last_system(hint.apply(ctx(("user", "There is no water in my area")))) == HINTS[ENGLISH]
    assert last_system(hint.apply(ctx(("assistant", "Is that right?"), ("user", "haan")))) == HINTS[ENGLISH]
    assert last_system(hint.apply(ctx(("assistant", "?"), ("user", "नहीं, मेरा नाम सुनीता वर्मा है")))) == HINTS[HINDI]
    assert last_system(hint.apply(ctx(("assistant", "?"), ("user", "ok")))) == HINTS[HINDI]


def test_a_turn_split_into_several_messages_is_read_as_one():
    hint = LanguageHint("multi")
    sent = hint.apply(ctx(("assistant", "?"), ("user", "My name"), ("user", "is Himanshu Verma, thank you")))
    assert last_system(sent) == HINTS[ENGLISH]


def test_nothing_added_before_the_caller_has_said_anything_clear():
    hint = LanguageHint("hi")
    c = ctx(("assistant", "नमस्ते"), ("user", "hello"))
    assert hint.apply(c) is c


def test_english_only_flows_are_left_alone():
    hint = LanguageHint("en")
    c = ctx(("user", "मेरे इलाके में तीन दिन से बिजली नहीं है।"))
    assert hint.apply(c) is c


def test_the_conversation_itself_is_not_changed():
    hint = LanguageHint("hi")
    c = ctx(("user", "It's fourteen august nineteen ninety."))
    before = len(c.items)
    sent = hint.apply(c)
    assert len(c.items) == before and len(sent.items) == before + 1


def test_the_agents_llm_adds_the_hint_to_every_request(monkeypatch):
    """agent.FlowLLM: the line reaches the request the plugin sends, for every step."""
    from livekit.plugins import openai

    import agent

    seen = []
    monkeypatch.setattr(openai.LLM, "chat", lambda self, *, chat_ctx, **kw: seen.append(chat_ctx) or "stream")
    model = agent.FlowLLM(flow_language="hi", model="voice-fast", api_key="test", base_url="http://localhost:1/v1")
    assert model.chat(chat_ctx=ctx(("user", "It's thirtieth September nineteen ninety six."))) == "stream"
    assert last_system(seen[0]) == HINTS[ENGLISH]
