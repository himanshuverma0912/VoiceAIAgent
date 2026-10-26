"""Which language to answer in, decided in code from what the caller just said.

A rule in the prompt ("reply in the language the caller is speaking") isn't
enough on its own: in test calls the agent kept answering in Hindi after the
caller spoke full English sentences, because the greeting and its own earlier
replies were in Hindi and a small model follows the conversation more than a
rule. So before every LLM request, LanguageHint looks at the caller's latest
words and adds one short instruction at the end: "Language for your next reply:
English" (or Hindi, or the caller's Hindi-English mix).

Only for flows whose language is Hindi or multilingual ("hi", "multi"); other
flows are left as their persona says. Nothing is stored or logged: the text is
only looked at in memory for this one request.
"""

from __future__ import annotations

import re

from livekit.agents import llm

ENGLISH, HINDI, MIXED = "en", "hi", "mixed"
BILINGUAL_FLOW_LANGUAGES = {"hi", "multi"}

_WORD_RE = re.compile(r"[A-Za-z']+|[ऀ-ॿ]+")
_DEVANAGARI_RE = re.compile(r"[ऀ-ॿ]")

# Common Hindi words as people (or speech-to-text) write them in Roman letters.
# Two of them in a Roman-letter sentence make it Hinglish rather than English.
_ROMAN_HINDI = {
    "hai", "hain", "nahi", "nahin", "mera", "meri", "mere", "mujhe", "kya", "aap", "aapka",
    "aapki", "aapke", "ka", "ki", "ke", "ko", "se", "raha", "rahi", "rahe", "gaya", "gayi",
    "kar", "karna", "karo", "kuch", "bhi", "haan", "ji", "accha", "acha", "theek", "thik",
    "hua", "tha", "thi", "paani", "bijli", "kab", "kyun", "kaise", "wala", "wali", "abhi",
    "yeh", "woh", "hum", "humara", "humko", "batao", "bataiye", "chahiye", "kijiye",
}

HINTS = {
    ENGLISH: (
        "Language for your next reply: English. The caller's latest words were in English, so "
        "reply only in English (no Hindi words, no Devanagari), including when you read back "
        "dates, names or numbers, even though the greeting or earlier replies were in Hindi."
    ),
    HINDI: (
        "Language for your next reply: Hindi, in Devanagari script. Common English words "
        "(EMI, loan, account, number, complaint) may stay in English."
    ),
    MIXED: (
        "Language for your next reply: the same Hindi-English mix the caller is using, with "
        "Hindi words in Devanagari script."
    ),
}


def detect(text: str) -> str | None:
    """English, Hindi or a mix; None when there's too little to tell (e.g. "ok", "haan")."""
    words = _WORD_RE.findall(text or "")
    if len(words) < 3:
        return None
    hindi_script = sum(1 for w in words if _DEVANAGARI_RE.match(w))
    roman = len(words) - hindi_script
    # A single Devanagari word in an English sentence is usually a name that
    # speech-to-text wrote in Hindi script ("my name is हिमांशु and my...").
    if hindi_script == 0 or (hindi_script == 1 and roman >= 4):
        roman_hindi = sum(1 for w in words if w.lower() in _ROMAN_HINDI)
        return MIXED if roman_hindi >= 2 else ENGLISH
    # Mostly Hindi script; a few English loanwords ("EMI", "loan") don't change that.
    if roman * 4 <= hindi_script:
        return HINDI
    return MIXED


def _callers_latest_words(chat_ctx: llm.ChatContext) -> str:
    """Everything the caller said since the agent last spoke (one turn can arrive
    as several messages)."""
    parts: list[str] = []
    for item in reversed(chat_ctx.items):
        if item.type != "message":
            continue
        if item.role == "assistant":
            break
        if item.role == "user":
            parts.append(item.text_content or "")
    return " ".join(reversed(parts))


class LanguageHint:
    """One per call. Remembers the last clear language, so a short "ok" keeps it."""

    def __init__(self, flow_language: str) -> None:
        self.enabled = flow_language in BILINGUAL_FLOW_LANGUAGES
        self.current: str | None = None

    def apply(self, chat_ctx: llm.ChatContext) -> llm.ChatContext:
        """The chat context to send: unchanged, or a copy with the language line at the end."""
        if not self.enabled:
            return chat_ctx
        detected = detect(_callers_latest_words(chat_ctx))
        if detected:
            self.current = detected
        if not self.current:
            return chat_ctx  # nothing clear yet: the persona's own rule decides
        ctx = chat_ctx.copy()
        ctx.add_message(role="system", content=HINTS[self.current])
        return ctx
