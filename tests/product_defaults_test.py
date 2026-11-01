"""Product defaults do not require environment tuning."""
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def test_provider_uses_path_not_private_override():
    # cliproxyapi is found on PATH (exec.LookPath); no code reads a private override.
    sources = [*ROOT.glob('cli/*.go'), *ROOT.glob('util/*.go'), *ROOT.glob('lib/*'), ROOT / 'bin/ccmorph']
    assert not [str(p) for p in sources if 'CCMORPH_PROVIDER_BIN' in p.read_text()]
    assert 'exec.LookPath("cliproxyapi")' in (ROOT / 'cli/run.go').read_text()
