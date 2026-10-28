"""Everything the router reads from the machine: its config, the profiles, live quota, prices, its ledger.

Credentials are read only to call the provider's own usage endpoint for that same
account, are never written, printed or put in a command line, and errors carry
only our own messages.
"""
from __future__ import annotations

import calendar
import contextlib
import datetime as dt
import hashlib
import json
import math
import os
from pathlib import Path
import re
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request

from .core import Profile, Window

try:
    import fcntl
except ImportError:   # shortcut: no lock on Windows, so parallel picks there can overlap; add msvcrt.locking if it matters
    fcntl = None

HOME = Path.home()
NAME = re.compile(r"[A-Za-z0-9_]+")
# The only profile keys ever read. Keys and tokens live in other keys and are never looked at.
SAFE_KEYS = {"type", "provider", "base_url", "model", "excluded_models", "context_discovered", "tag"}
TIER_KEYS = {f"env.ANTHROPIC_DEFAULT_{t}_MODEL" for t in ("OPUS", "SONNET", "HAIKU", "FABLE")}
# What an Anthropic subscription serves through Claude Code's tier aliases.
ANTHROPIC_SUBSCRIPTION_MODELS = ["claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"]
# API-equivalent dollars one window holds, by plan. Rough public figures; `capacity_usd` in a profile's entry overrides them.
PLAN_CAPACITY = {
    "pro": {"5h": 12, "7d": 100}, "max_5x": {"5h": 60, "7d": 500}, "max_20x": {"5h": 240, "7d": 2000},
    "chatgpt_plus": {"5h": 10, "7d": 80}, "chatgpt_pro": {"5h": 100, "7d": 800},
}
WINDOW_LENGTH = {"5h": 5 * 3600, "7d": 7 * 86400}
QUOTA_TTL = 300
MODELS_DEV_URL = "https://models.dev/api.json"
MODELS_DEV_TTL = 7 * 86400
PROVIDER_ORDER = ("anthropic", "openai", "google", "deepseek", "moonshotai", "zai", "xai", "alibaba")


def config_dir() -> Path:
    return Path(os.environ.get("XDG_CONFIG_HOME") or HOME / ".config") / "model-router"


def cache_dir() -> Path:
    return Path(os.environ.get("XDG_CACHE_HOME") or HOME / ".cache") / "model-router"


def state_dir() -> Path:
    return Path(os.environ.get("XDG_STATE_HOME") or HOME / ".local" / "state") / "model-router"


def config_path() -> Path:
    return Path(os.environ.get("MODEL_ROUTER_CONFIG") or config_dir() / "config.json")


def ccmorph_dir() -> Path:
    return Path(os.environ.get("CCMORPH_STATE_DIR") or
                Path(os.environ.get("XDG_CONFIG_HOME") or HOME / ".config") / "ccmorph")


def write_json(path: Path, data) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    tmp = path.with_suffix(f"{path.suffix}.{os.getpid()}.tmp")   # parallel hooks each write their own
    tmp.write_text(json.dumps(data, indent=2) + "\n")
    tmp.replace(path)


def read_json(path: Path, default=None):
    try:
        return json.loads(path.read_text())
    except (OSError, ValueError):
        return default


# ---------- profiles (ccmorph) ----------

def read_conf(path: Path) -> dict:
    fields = {}
    for line in path.read_text(encoding="utf-8").split("\n"):
        key, sep, value = line.strip(" \t\r").partition("=")
        if sep and (key in SAFE_KEYS or key in TIER_KEYS):
            fields[key] = value
    return fields


def bare(model: str) -> str:
    return model[:-4] if model.endswith("[1m]") else model


def discover() -> dict:
    """Every ccmorph profile as a router profile entry: class, quota source and models."""
    found = {}
    root = ccmorph_dir()
    for path in sorted(root.glob("*.conf")):
        name = path.stem
        if not NAME.fullmatch(name) or path.is_symlink():
            continue
        try:
            conf = read_conf(path)
        except OSError:
            continue
        kind, provider = conf.get("type", "apikey"), conf.get("provider", "")
        if kind in ("sub", "token"):
            # statusline is Claude Code's documented source; the OAuth usage endpoint is opt-in (`quota: anthropic`)
            entry = {"class": "subscription", "quota": "statusline",
                     "models": list(ANTHROPIC_SUBSCRIPTION_MODELS)}
        else:
            models = set()
            try:
                models |= set(json.loads(conf.get("context_discovered") or "{}").get("models", {}))
            except ValueError:
                pass
            models |= {conf[k] for k in TIER_KEYS if conf.get(k)}
            if conf.get("model"):
                models.add(conf["model"])
            excluded = set(filter(None, conf.get("excluded_models", "").split(",")))
            models = sorted({bare(m) for m in models} - excluded)
            if kind == "provider" and provider == "codex":
                entry = {"class": "subscription", "quota": "codex", "models": models}
            else:
                entry = {"class": "api", "quota": "none", "models": models}
        entry["source"] = "ccmorph"
        found[name] = entry
    return found


def session_profile() -> str:
    """This Claude Code session's profile: the name of its config dir, or `default` for ~/.claude."""
    name = Path(os.environ.get("CLAUDE_CONFIG_DIR", "")).name
    return name if NAME.fullmatch(name) else "default"


# ---------- config ----------

DEFAULTS = {
    "priority": ["subscription", "budget", "api"],
    "mode": "balanced",
    "protect_above": 0.5,
    "reserve": {"5h": 0.15, "7d": 0.1, "month": 0.05},
    "decider": {"url": "http://127.0.0.1:8099/v1/systemone"},
    "executor": "agent-dispatch",
    "profiles": {},
}


def load_config() -> dict:
    cfg = json.loads(json.dumps(DEFAULTS))
    cfg.update(read_json(config_path(), {}) or {})
    return cfg


# ---------- prices and context (models.dev) ----------

def models_dev(refresh: bool = False) -> dict:
    path = cache_dir() / "models.dev.json"
    stale = not path.exists() or time.time() - path.stat().st_mtime > MODELS_DEV_TTL
    if refresh or stale:
        try:
            req = urllib.request.Request(MODELS_DEV_URL, headers={"user-agent": "model-router"})
            with urllib.request.urlopen(req, timeout=20) as r:
                body = r.read(64 * 1024 * 1024)
            json.loads(body)
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(body)
        except (OSError, ValueError):
            pass   # offline: keep the last copy
    return read_json(path, {}) or {}


def model_facts(cat: dict, data: dict) -> tuple[dict, dict]:
    """(input types, context) per catalogue model, from models.dev."""
    inputs, contexts = {}, {}
    ordered = [p for p in PROVIDER_ORDER if p in data] + [p for p in data if p not in PROVIDER_ORDER]
    for model, entry in cat["models"].items():
        if entry.get("inputs"):
            inputs[model] = tuple(entry["inputs"])   # offline fallback: a fresh install has no models.dev copy yet
        for cand in [model, *entry.get("aliases", [])]:
            hit = next((data[p]["models"][cand] for p in ordered
                        if cand in data[p].get("models", {}) and data[p]["models"][cand].get("cost")), None)
            if hit:
                inputs[model] = tuple(hit.get("modalities", {}).get("input", ("text",)))
                contexts[model] = hit.get("limit", {}).get("context", 0)
                break
    return inputs, contexts


def resolve(cat: dict, model: str) -> str | None:
    """The catalogue key for a profile's model id, through aliases and date suffixes."""
    model = bare(model)
    if model in cat["models"]:
        return model
    for key, entry in cat["models"].items():
        if model in entry.get("aliases", []):
            return key
    trimmed = re.sub(r"-(\d{8}|\d{4})$", "", model)
    return trimmed if trimmed in cat["models"] else None


def own_id(cat: dict, key: str, served: list) -> str:
    """The id a profile serves a catalogue model under: the key itself, else the first catalogue alias it serves."""
    order = [key, *cat["models"][key].get("aliases", [])]
    return min(served, key=lambda m: order.index(bare(m)) if bare(m) in order else len(order))


# ---------- live quota ----------

def tls() -> ssl.SSLContext:
    """Default trust, the macOS system roots (/etc/ssl/cert.pem lacks some), and any CA file the environment names."""
    ctx = ssl.create_default_context()
    if sys.platform == "darwin":
        pem = cache_dir() / "system-roots.pem"
        if not pem.exists() or time.time() - pem.stat().st_mtime > 30 * 86400:
            try:
                out = subprocess.run(["security", "find-certificate", "-a", "-p",
                                      "/System/Library/Keychains/SystemRootCertificates.keychain"],
                                     capture_output=True, text=True, timeout=10).stdout
                if "BEGIN CERTIFICATE" in out:
                    pem.parent.mkdir(parents=True, exist_ok=True)
                    pem.write_text(out)
            except (OSError, subprocess.SubprocessError):
                pass
        if pem.exists():
            ctx.load_verify_locations(str(pem))
    for var in ("SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"):   # extra trust a proxy in this environment declares
        if os.environ.get(var) and Path(os.environ[var]).is_file():
            ctx.load_verify_locations(os.environ[var])
    return ctx


class NoRedirect(urllib.request.HTTPRedirectHandler):
    """A bearer token never follows a redirect: a 3xx from a usage endpoint is an error."""
    def redirect_request(self, *args, **kwargs):
        return None


def _get(url: str, headers: dict, timeout: float = 8):
    req = urllib.request.Request(url, headers={**headers, "accept": "application/json", "user-agent": "model-router"})
    proxy = urllib.request.ProxyHandler() if os.environ.get("HTTPS_PROXY") else urllib.request.ProxyHandler({})
    opener = urllib.request.build_opener(proxy, urllib.request.HTTPSHandler(context=tls()), NoRedirect())
    with opener.open(req, timeout=timeout) as r:
        return json.loads(r.read(1024 * 1024))


def finite(value) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def share(percent) -> float | None:
    """A provider's percentage as a 0..1 share, or None when it is not a finite number."""
    return min(max(percent / 100, 0.0), 1.0) if finite(percent) else None


def _iso(value) -> float | None:
    if finite(value):
        return float(value)
    if isinstance(value, str) and value:
        try:
            return dt.datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()
        except ValueError:
            return None
    return None


def anthropic_login(name: str) -> dict | None:
    """The OAuth login Claude Code keeps for this profile's config dir: macOS keychain, else .credentials.json."""
    profile_dir = ccmorph_dir() / "profiles" / name
    svc = "Claude Code-credentials-" + hashlib.sha256(str(profile_dir).encode()).hexdigest()[:8]
    raw = None
    try:
        raw = subprocess.run(["security", "find-generic-password", "-s", svc, "-w"], capture_output=True,
                             text=True, timeout=8).stdout
    except (OSError, subprocess.SubprocessError):
        pass
    if not raw:
        try:
            raw = (profile_dir / ".credentials.json").read_text()
        except OSError:
            return None
    try:
        return json.loads(raw).get("claudeAiOauth")
    except ValueError:
        return None


def read_anthropic(name: str) -> dict:
    login = anthropic_login(name)
    if not login or not login.get("accessToken"):
        return {"note": "no Claude Code sign-in found for this profile"}
    if login.get("expiresAt") and login["expiresAt"] / 1000 < time.time():
        return {"note": "the sign-in has expired; open Claude Code on this profile once"}
    try:
        body = _get("https://api.anthropic.com/api/oauth/usage",
                    {"authorization": "Bearer " + login["accessToken"], "anthropic-beta": "oauth-2025-04-20"})
    except urllib.error.HTTPError as e:
        return {"note": f"Anthropic usage endpoint answered {e.code}"}
    except (OSError, ValueError):
        return {"note": "Anthropic usage endpoint could not be read"}
    if not isinstance(body, dict):
        return {"note": "Anthropic usage endpoint gave an unexpected answer"}
    tier = str(login.get("rateLimitTier") or "").lower()
    plan = "max_20x" if "20x" in tier else "max_5x" if "5x" in tier or "max" in str(login.get("subscriptionType") or "") else "pro"
    windows = []
    for key, value in body.items():
        used = share(value.get("utilization")) if isinstance(value, dict) else None
        if used is None:
            continue
        if key == "five_hour":
            wname, models = "5h", ()
        elif key == "seven_day":
            wname, models = "7d", ()
        elif key.startswith("seven_day_") and key[10:] in ("opus", "sonnet"):
            wname, models = "7d-" + key[10:], (key[10:],)
        else:
            continue
        base = wname[:2]
        windows.append({"name": wname, "used": used,
                        "resets_at": _iso(value.get("resets_at")) or time.time() + WINDOW_LENGTH[base],
                        "length": WINDOW_LENGTH[base], "capacity_usd": PLAN_CAPACITY[plan][base], "models": models})
    return {"plan": plan, "windows": windows}


def codex_login(name: str) -> dict | None:
    found = []
    for path in sorted((ccmorph_dir() / "auth" / name).glob("*.json")):
        data = read_json(path)
        if isinstance(data, dict) and data.get("type") == "codex" and not data.get("disabled") \
                and data.get("access_token") and data.get("account_id"):
            found.append({"token": data["access_token"], "account": data["account_id"]})
    if not found:
        native = read_json(HOME / ".codex" / "auth.json") or {}
        tokens = native.get("tokens") or {}
        if tokens.get("access_token") and tokens.get("account_id") and name == "codex-cli":
            found.append({"token": tokens["access_token"], "account": tokens["account_id"]})
    return found[0] if len(found) == 1 else None   # a pool has no single quota


def read_codex(name: str) -> dict:
    login = codex_login(name)
    if not login:
        return {"note": "no single Codex sign-in found for this profile"}
    try:
        body = _get("https://chatgpt.com/backend-api/wham/usage",
                    {"authorization": "Bearer " + login["token"], "chatgpt-account-id": login["account"]})
    except urllib.error.HTTPError as e:
        return {"note": f"Codex usage endpoint answered {e.code}"}
    except (OSError, ValueError):
        return {"note": "Codex usage endpoint could not be read"}
    if not isinstance(body, dict) or not isinstance(body.get("rate_limit") or {}, dict):
        return {"note": "Codex usage endpoint gave an unexpected answer"}
    plan = "chatgpt_pro" if body.get("plan_type") == "pro" else "chatgpt_plus"
    windows = []
    for key in ("primary_window", "secondary_window"):
        w = (body.get("rate_limit") or {}).get(key)
        length = w.get("limit_window_seconds") if isinstance(w, dict) else None
        used = share(w.get("used_percent")) if length in (18000, 604800) else None
        if used is None:
            continue
        wname = "5h" if length == 18000 else "7d"
        after = w.get("reset_after_seconds")
        reset = _iso(w.get("reset_at")) or (time.time() + after if finite(after) else None)
        windows.append({"name": wname, "used": used, "resets_at": reset or time.time() + length, "length": length,
                        "capacity_usd": PLAN_CAPACITY[plan][wname], "models": ()})
    return {"plan": plan, "windows": windows}


def month_window(limit: float, spent: float) -> dict:
    now = dt.datetime.now().astimezone()
    days = calendar.monthrange(now.year, now.month)[1]
    start = now.replace(day=1, hour=0, minute=0, second=0, microsecond=0)
    end = start + dt.timedelta(days=days)
    return {"name": "month", "used": min(spent / limit, 1), "resets_at": end.timestamp(),
            "length": days * 86400, "capacity_usd": limit, "models": ()}


READERS = {"anthropic": read_anthropic, "codex": read_codex}


def quota_path(name: str) -> Path:
    return cache_dir() / "quota" / (re.sub(r"[^A-Za-z0-9_.-]", "_", name) + ".json")   # one file per profile: no lost updates


def cached(name: str) -> dict | None:
    hit = read_json(quota_path(name))
    return hit if isinstance(hit, dict) else None


def stamp(old: dict | None, windows: list) -> float:
    """When the reading last moved. A redraw that shows the same use keeps the old time, so work routed since stays pending."""
    def use(ws):
        return [(w["name"], w["used"]) for w in ws]
    return old["as_of"] if old and old.get("as_of") and use(old.get("windows", [])) == use(windows) else time.time()


def record_statusline(payload: dict, name: str) -> dict | None:
    """Store the documented `rate_limits` block Claude Code hands its statusline command."""
    limits = payload.get("rate_limits") if isinstance(payload, dict) else None
    windows = []
    for key, wname in (("five_hour", "5h"), ("seven_day", "7d")):
        w = limits.get(key) if isinstance(limits, dict) else None
        used = share(w.get("used_percentage")) if isinstance(w, dict) else None
        if used is not None:
            windows.append({"name": wname, "used": used,
                            "resets_at": _iso(w.get("resets_at")) or time.time() + WINDOW_LENGTH[wname],
                            "length": WINDOW_LENGTH[wname], "capacity_usd": PLAN_CAPACITY["pro"][wname], "models": ()})
    if not windows:
        return None
    entry = {"as_of": stamp(cached(name), windows), "checked": time.time(), "windows": windows,
             "source": "statusline", "note": None}
    write_json(quota_path(name), entry)
    return entry


def aged(entry: dict) -> dict:
    """A stored reading as it stands now: a window whose reset has passed starts again empty."""
    now = time.time()
    windows = []
    for w in entry.get("windows", []):
        if w["resets_at"] <= now:
            periods = int((now - w["resets_at"]) // w["length"]) + 1
            w = {**w, "used": 0.0, "resets_at": w["resets_at"] + periods * w["length"]}
        windows.append(w)
    return {**entry, "windows": windows}


def quota(name: str, entry: dict, refresh: bool = False) -> dict:
    """{"as_of", "windows", "note"} for one profile, cached for QUOTA_TTL seconds."""
    if entry.get("class") == "budget":
        limit = float(entry.get("monthly_budget_usd") or 0)
        if limit <= 0:
            return {"as_of": time.time(), "windows": [], "note": "no monthly_budget_usd set"}
        spent = float(entry.get("spent_usd") or 0) + ledger_spend(name, month=True)
        return {"as_of": time.time(), "windows": [month_window(limit, spent)], "note": None}
    hit = cached(name)
    if entry.get("quota") == "statusline":
        if not hit:
            return {"as_of": None, "windows": [], "note": "no statusline reading yet; see README, Quota sources"}
        return aged(hit)
    reader = READERS.get(entry.get("quota"))
    if not reader:
        return {"as_of": None, "windows": [], "note": None}
    if hit and not refresh and time.time() - hit.get("checked", 0) < QUOTA_TTL:
        return aged(hit)
    got = reader(name)
    if got.get("windows"):
        hit = {"as_of": stamp(hit, got["windows"]), "checked": time.time(), "windows": got["windows"],
               "plan": got.get("plan"), "note": None}
    else:   # keep the last good reading, aged, with the reason
        hit = {**(hit or {"as_of": None, "windows": []}), "checked": time.time(), "note": got.get("note")}
    write_json(quota_path(name), hit)
    return aged(hit)


def build_profiles(cfg: dict, cat: dict, refresh: bool = False, only: set | None = None) -> tuple[list, dict]:
    """Router profiles for every enabled entry, with live windows; plus per-profile notes."""
    profiles, notes = [], {}
    for name, entry in cfg.get("profiles", {}).items():
        if not entry.get("enabled", True) or (only and name not in only):
            continue
        served = {}
        for m in entry.get("models", []):
            key = resolve(cat, m)
            if key and key not in entry.get("disabled_models", []):
                served.setdefault(key, []).append(m)
        klass = entry.get("class", "api")
        windows, known = [], True
        if klass in ("subscription", "budget"):
            q = quota(name, entry, refresh)
            capacity = entry.get("capacity_usd") or PLAN_CAPACITY.get(entry.get("plan"), {})
            for w in q.get("windows", []):
                if w["name"][:2] in capacity:
                    w = {**w, "capacity_usd": capacity[w["name"][:2]]}
                windows.append(Window(w["name"], w["used"], w["resets_at"], w["length"], w.get("capacity_usd"), tuple(w.get("models", ()))))
            known = bool(windows)
            if q.get("note"):
                notes[name] = q["note"]
            pending = ledger_spend(name, since=q.get("as_of") or 0, horizon=2 * 3600)
        else:
            pending = 0.0
        reserve = {**cfg.get("reserve", {}), **entry.get("reserve", {})}
        reserve = {**reserve, **{f"7d-{f}": reserve.get("7d", 0) for f in ("opus", "sonnet")}}
        profiles.append(Profile(name, klass, list(served), windows, reserve, pending, known,
                                float(entry.get("price_factor", 1.0)),
                                {key: own_id(cat, key, ids) for key, ids in served.items()}))
    return profiles, notes


# ---------- ledger ----------

def ledger_path() -> Path:
    return state_dir() / "ledger.jsonl"


def ledger_rows() -> list:
    try:
        lines = ledger_path().read_text().splitlines()
    except OSError:
        return []
    rows = []
    for line in lines:
        try:
            rows.append(json.loads(line))
        except ValueError:
            continue
    return rows


@contextlib.contextmanager
def ledger_lock():
    """One routing decision at a time across processes, so parallel picks see each other's reservations."""
    path = state_dir() / "ledger.lock"
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    with path.open("a") as f:
        if fcntl:
            fcntl.flock(f, fcntl.LOCK_EX)
        yield


def ledger_append(row: dict) -> None:
    path = ledger_path()
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    with path.open("a") as f:
        f.write(json.dumps(row) + "\n")


def ledger_spend(profile: str, since: float = 0, horizon: float | None = None, month: bool = False) -> float:
    now = time.time()
    start = dt.datetime.now().astimezone().replace(day=1, hour=0, minute=0, second=0, microsecond=0).timestamp() if month else 0
    total = 0.0
    for row in ledger_rows():
        ts = row.get("ts", 0)
        if row.get("profile") != profile or ts <= since or ts < start or (horizon and ts < now - horizon):
            continue
        total += float(row.get("cost_usd") or 0)
    return total
