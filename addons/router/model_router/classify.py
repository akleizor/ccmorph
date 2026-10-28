"""Task understanding: what kind of work, how hard, what it needs.

The decider is any endpoint speaking the typed decision contract (POST
{state, questions} -> answers with probabilities), such as a local
pplx-decider: one forward pass, no generation, nothing billed. Without one,
keyword rules give a rougher answer. The caller can pin any field.
"""
from __future__ import annotations

import json
import re
import urllib.error
import urllib.request

from .core import Task

MAX_CHARS = 7000   # the local decider holds 4,096 tokens; questions take about 400


def questions(cat: dict) -> dict:
    return {
        "kind": {"type": "choice", "instructions": "What kind of work is this task mainly?",
                 "criteria": {k: v["label"] for k, v in cat["kinds"].items()}},
        "difficulty": {"type": "score", "instructions": "How hard is this task for a strong senior engineer?",
                       "criteria": cat["difficulty"]["levels"]},
        "needs_vision": {"type": "noul", "instructions": "Does the task need to look at images, screenshots, PDFs or video?"},
        "needs_web": {"type": "noul", "instructions": "Does the task need live internet access?"},
        "needs_x": {"type": "noul", "instructions": "Does the task need posts from X (formerly Twitter)?"},
        "self_contained": {"type": "noul", "instructions":
                           "Could a fresh assistant do this from the text alone, without the earlier conversation?"},
    }


def ask_decider(url: str, prompt: str, cat: dict, timeout: float = 30) -> Task:
    text = prompt if len(prompt) <= MAX_CHARS else prompt[:MAX_CHARS // 2] + "\n...\n" + prompt[-MAX_CHARS // 2:]
    body = json.dumps({"state": {"task": text}, "questions": questions(cat)}).encode()
    req = urllib.request.Request(url, data=body, headers={"content-type": "application/json"})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(req, timeout=timeout) as r:
        answers = json.loads(r.read())["answers"]
    probs = {k: float(v) for k, v in answers["kind"]["probabilities"].items() if k in cat["kinds"]}
    kinds = {k: p for k, p in probs.items() if p >= 0.05} or {answers["kind"]["choice"]: 1.0}
    total = sum(kinds.values())
    needs = {n for n in ("vision", "web", "x") if answers[f"needs_{n}"]["noul"] >= 0.5}
    offset, scale = cat.get("decider_scale", (0.0, 1.0))   # the decider's scores are right in order but squeezed toward the middle
    level = min(max(offset + scale * float(answers["difficulty"]["score"]), 0.0), 4.0)
    return Task({k: p / total for k, p in kinds.items()}, level, needs,
                source="decider", self_contained=float(answers["self_contained"]["noul"]))


RULES = [
    ("social_search", r"\b(twitter|tweet|x\.com|reddit|小红书|xiaohongshu|threads|social media)\b"),
    ("vision", r"\b(screenshot|image|photo|picture|diagram|pdf|video|figma|mockup)\b"),
    ("web_research", r"\b(search|research|look up|find out|latest|news|compare|survey|docs? for)\b"),
    ("code_review", r"\b(review|audit|critique|check (this|the) (diff|pr|code))\b"),
    ("writing", r"\b(write|draft|rewrite|email|blog|readme|report|summari[sz]e|translate)\b"),
    ("analysis", r"\b(why|debug|root cause|plan|design|architecture|prove|math|estimate)\b"),
    ("mechanical", r"\b(rename|bulk|every file|all files|grep|list|format|move|copy|bump)\b"),
    ("code_edit", r"\b(fix|implement|refactor|add|bug|test|function|class|endpoint|code)\b"),
]
X_POSTS = r"\b(twitter|tweets?|x\.com)\b|\bsearch x\b|\bposts? (on|from) x\b"
HARD = r"\b(architecture|design|refactor|concurren|race|security|migrat|distributed|optimi[sz]|prove|from scratch)\b"
EASY = r"\b(rename|typo|list|bump|format|one line|small|quick|simple|grep)\b"


def heuristic(prompt: str) -> Task:
    text = prompt.lower()
    hits = {k: len(re.findall(p, text)) for k, p in RULES}
    hits = {k: n for k, n in hits.items() if n}
    if not hits:
        hits = {"code_edit": 1}
    total = sum(hits.values())
    level = 2.0 + 0.6 * len(re.findall(HARD, text)) - 0.8 * len(re.findall(EASY, text)) + min(len(prompt) / 4000, 1.0)
    needs = {"vision"} if "vision" in hits else set()
    if "web_research" in hits or "social_search" in hits:
        needs.add("web")
    if re.search(X_POSTS, text):
        needs |= {"web", "x"}
    return Task({k: n / total for k, n in hits.items()}, min(max(level, 0.0), 4.0), needs, source="heuristic")


def classify(prompt: str, cat: dict, decider_url: str | None, timeout: float = 30) -> tuple[Task, str | None]:
    """The task, and a note when the decider could not be used."""
    if decider_url:
        try:
            return ask_decider(decider_url, prompt, cat, timeout), None
        except (OSError, ValueError, KeyError, TypeError) as e:
            reason = f"decider answered {e.code}" if isinstance(e, urllib.error.HTTPError) else "decider unavailable"
            return heuristic(prompt), reason + "; used keyword rules"
    return heuristic(prompt), None
