# Install experience — task plan

Status: **proposed** (Hive rule in AGENTS.md: a new user-facing behaviour is a product
decision and merges only after Alex approves). Work happens on branch
`claude/festive-maxwell-dj0f8e` in the worktree `.claude/worktrees/install-experience`.
This file is the hand-off from the planning pass to the implementers; it is removed
from the branch (or moved to the vault) before the PR merges.

## Goal

Make installing ccmorph feel like installing a mainstream CLI — one line, works on
macOS and Linux (including WSL), says exactly what it did and what to do next — while
keeping every boundary this repo already enforces. Nothing here changes what a launch
does, which backend a profile reaches, or the model picker.

Before and after, as the user sees it:

```
# today
git clone https://github.com/akleizor/ccmorph && cd ccmorph && ./install.sh
ccmorph --version

# after this plan
sh -c "$(curl -fsSL https://raw.githubusercontent.com/akleizor/ccmorph/main/install.sh)"
ccmorph doctor          # environment report, no profile needed
ccmorph add work        # first profile becomes the default
ccmorph run
```

## Decisions already made (do not reopen them while implementing)

1. **`install.sh` stays the one entry point.** No second bootstrap script, no `get.sh`.
   The one-liner fetches `install.sh` itself; a "remote mode" inside it downloads the
   tree and runs the downloaded copy.
2. **One-liner form is `sh -c "$(curl -fsSL <url>)"`** (Homebrew / oh-my-zsh pattern),
   because it keeps stdin on the terminal so the optional-dependency offers can still
   ask. `curl ... | sh` must also work; it simply declines the offers (no tty), which is
   already the documented non-interactive behaviour.
3. **Source of the download:** `https://github.com/akleizor/ccmorph/archive/<ref>.tar.gz`,
   `<ref>` = `$CCMORPH_REF`, default `main`. There are no tags or GitHub releases today
   (checked 2026-10-27), so `main` is the only ref that exists; pinning to a tag becomes
   possible the day one is pushed (Alex's call, see "Out of scope"). `CCMORPH_INSTALL_URL`
   overrides the whole URL — this is what the tests and CI use to stay off the network.
4. **Required dependencies (`zsh`, `python3 >= 3.8`, `claude`) are named, never installed.**
   They need root or belong to another vendor. The installer prints the exact command
   for the detected package manager and stops. **Optional dependencies (`fzf`,
   `cliproxyapi`) are offered** through the detected package manager, confirm-gated,
   declined without a tty — the existing `_offer` contract, extended beyond brew.
5. **Shell startup files are never edited.** PATH advice gets smarter (per shell), it
   does not get automatic. `ARCHITECTURE.md#scope-and-ownership` is the authority.
6. **No Homebrew formula.** It was removed deliberately (CHANGELOG, "The Homebrew formula
   is gone": no public tap). Do not propose one in docs or code.
7. **`ccmorph update` is deferred** (Phase C, marked optional). It needs install.sh to be
   installed as a component and a `--remote` flag; do it last, only if Alex wants it.
8. Every task ships with a test that fails before the change (AGENTS.md). A task
   without its test is not done.

## Out of scope (say so in the PR, do not do it)

- Installing `claude`, `zsh` or `python3` on the user's behalf.
- Editing `.zshrc`/`.bashrc`/fish config, or adding to PATH automatically.
- A Homebrew tap/formula, a `.deb`, a Windows-native build.
- Tagging a release. Note for Alex: `make check-version` wants a `v2.5.1` tag; once a tag
  exists, `CCMORPH_REF=v2.5.1` pins the one-liner to it with no code change.
- Fixing the two pre-existing Python test failures seen on Linux-as-root
  (`adapter_test.py::test_an_unwritable_cache_dir_degrades...` needs a non-root user;
  `fake_adapter_contract_test.py::test_reap_ends_a_fake_whose_parent_never_dies_issue_347`
  is `ps -o command=` truncating to 80 columns on procps — add `-ww`). Separate PR.

## Rules for every task

Read first, once: `AGENTS.md`, `REVIEW.md`, `ARCHITECTURE.md#scope-and-ownership`,
`install.sh` top to bottom, `tests/install_test.zsh` top to bottom.

- Work only in `/home/user/ccmorph/.claude/worktrees/install-experience` (or wherever this
  worktree is checked out). Never touch `~/.config/ccmorph` and never run `install.sh`
  against a real `$HOME`.
- The block at the top of `install.sh` before `set -eu` runs under **whatever shell the
  user typed** (`sh`, `bash`, `dash`, `zsh`). Only POSIX sh syntax there: no arrays, no
  `[[ ]]`, no `${var:A}`, no `(N)` globs, no `local`, quote every expansion. Everything
  after the zsh re-exec is zsh and may use the repo's idioms.
- `install.sh` runs `set -eu`. Any command that may legitimately fail must end in
  `|| true` or be inside an `if`. A declined offer is not a failure.
- Tests in `tests/install_test.zsh` are hermetic: `env -i`, fake `$HOME`, a fake `bin/`
  dir first on `PATH`, `</dev/null` so nothing blocks. **No network.** A test that would
  download must get a stub `curl`/`wget` in the fake bin dir.
- Keep the installed surface exactly the manifest: the "install places exactly its own
  manifest" case lists every file; adding a component means adding it to `want=` there
  and to `COMPONENTS` in `install.sh`, both.
- No host-specific paths or product names in shipped source
  (`test_source_carries_no_host_specific_defaults`, `test_shipped_surface_never_names_the_transport_product`).
- Messages: one line that says what happened and what to do, in the voice already used
  in `install.sh` (`c_ok`, `c_y`, `c_r`, `c_dim`). No emoji, no banners.
- After every task: `zsh -n install.sh`, `zsh tests/install_test.zsh`, plus the Python
  file you touched (`CCMORPH_LIB_DIR="$PWD/lib" uv run --with pytest --with pytest-timeout python -m pytest tests/<file>.py -q`).
  Before the PR, once: the four suites named in AGENTS.md.
- One commit per task, message in the repo's style (`feat(install): …`, `fix(cli): …`,
  `test(install): …`, `docs: …`), body explaining why in two or three sentences, ending
  with the attribution lines your session gives you.
- When a task's instructions and the code disagree, stop and report; do not improvise
  around a boundary.

## Environment for the implementer

- Needs `zsh`, `python3`, `uv` (or pytest), `tar`, `curl`. The cloud container had no zsh:
  `apt-get update && apt-get install -y zsh` (dpkg may print an unrelated error; `zsh --version` is the check).
- Baseline on `ff7d07b` (Linux, root): all zsh suites green; Python 985 passed, 2 failed
  (the two environmental failures above). Anything else red after your change is yours.

---

## Phase A — `install.sh`: one line, any environment

Tasks A1–A5 all edit `install.sh`; do them in order, one commit each.

### A1. Remote mode: `install.sh` installs from a download when it is not inside a tree

**Why.** `sh -c "$(curl … install.sh)"` runs the script with `$0 = sh` and no repo
beside it. Today `SRC="${0:A:h}"` then points at nowhere and every `cp` fails.

**Files.** `install.sh`, `tests/install_test.zsh`.

**Design.** Insert a POSIX block *before* the existing `if [ -z "${ZSH_VERSION:-}" ]`
re-exec block (so `curl … | zsh` is covered too). Decide local vs remote by `$0`:

```sh
# ---- remote mode (POSIX sh: runs under whatever shell started us) -----------
# `sh -c "$(curl -fsSL .../install.sh)"` and `curl ... | sh` run this script with
# $0 = sh and no tree beside it. Fetch the tree, then run ITS installer.
_ccm_local=0
case "$0" in
  */install.sh|install.sh)
    [ -r "$0" ] && [ -r "$(dirname "$0")/lib/ccmorph.sh" ] && _ccm_local=1 ;;
esac
if [ "$_ccm_local" = 0 ] || [ "${1:-}" = "--remote" ]; then
  [ "${1:-}" = "--remote" ] && shift
  _ccm_ref="${CCMORPH_REF:-main}"
  _ccm_url="${CCMORPH_INSTALL_URL:-https://github.com/akleizor/ccmorph/archive/${_ccm_ref}.tar.gz}"
  _ccm_tmp="$(mktemp -d "${TMPDIR:-/tmp}/ccmorph-install.XXXXXX")" || exit 1
  _ccm_fail() { echo "ccmorph: $1" >&2; rm -rf "$_ccm_tmp"; exit 1; }
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$_ccm_url" -o "$_ccm_tmp/src.tar.gz" || _ccm_fail "download failed: $_ccm_url"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$_ccm_tmp/src.tar.gz" "$_ccm_url" || _ccm_fail "download failed: $_ccm_url"
  else
    _ccm_fail "neither curl nor wget is on PATH; install one, or clone the repo and run ./install.sh"
  fi
  tar -xzf "$_ccm_tmp/src.tar.gz" -C "$_ccm_tmp" || _ccm_fail "could not unpack $_ccm_url"
  _ccm_src=""
  for _ccm_d in "$_ccm_tmp"/*/; do
    [ -r "${_ccm_d}lib/ccmorph.sh" ] && [ -r "${_ccm_d}install.sh" ] && _ccm_src="$_ccm_d"
  done
  [ -n "$_ccm_src" ] || _ccm_fail "the download is not a ccmorph source tree: $_ccm_url"
  echo "ccmorph: installing from $_ccm_url"
  sh "${_ccm_src}install.sh" "$@"
  _ccm_rc=$?
  rm -rf "$_ccm_tmp"
  exit $_ccm_rc
fi
```

Then the existing zsh re-exec block, unchanged. Notes:
- `--remote` is the explicit switch a later `ccmorph update` can use; it costs nothing now.
- The downloaded installer runs as `sh …/install.sh` → `$0` ends in `install.sh` and has
  `lib/ccmorph.sh` beside it → local mode → re-execs zsh as today. No recursion.
- `--uninstall` in remote mode: the downloaded tree's installer handles it. Fine.
- Keep the whole block free of zsh-isms; it is parsed by `zsh -n` in CI as well, which
  is fine for POSIX code.

**Tests to write first** (new section `# --- 8. remote mode` in `tests/install_test.zsh`,
before the final summary). Build one tarball once, shaped like GitHub's
(`<topdir>/install.sh`, `<topdir>/lib/...`):

```zsh
stage="$TMP/stage/ccmorph-main"; mkdir -p "$stage"
cp -R "$REPO"/{bin,lib,libexec,share} "$REPO"/{install.sh,local.sh.example,profile.conf.example} "$stage/"
tar -czf "$TMP/src.tar.gz" -C "$TMP/stage" ccmorph-main
```

Stub `curl` in the case's bin dir: records its argv to `$d/curl.argv`, finds the
argument after `-o`, copies `$TMP/src.tar.gz` there, exits 0. Cases:

1. `sh -c "$(cat $REPO/install.sh)"` with `env -i HOME=… PATH="$d/bin:/usr/bin:/bin"`
   `</dev/null` → exit 0; `~/.config/ccmorph/lib/ccmorph.sh` exists; `~/.local/bin/ccmorph`
   exists; `curl.argv` contains `https://github.com/akleizor/ccmorph/archive/main.tar.gz`.
2. Same via a pipe: `cat $REPO/install.sh | env -i … sh` → exit 0, same files.
3. `CCMORPH_REF=v9.9.9` → `curl.argv` contains `archive/v9.9.9.tar.gz`.
4. `CCMORPH_INSTALL_URL=file:///x/y.tgz` → that exact URL reaches curl.
5. No `curl` and no `wget` in PATH (PATH=`$d/bin` only, with `sh`, `tar`, `mktemp`,
   `dirname`, `rm`, `mkdir` symlinked in from `/usr/bin` or `/bin`) → exit 1, stderr
   names both `curl` and `wget`.
6. Tarball without `lib/ccmorph.sh` (tar of an empty dir) → exit 1, message says it is
   not a ccmorph tree; `$TMP/ccmorph-install.*` left behind: none (`ls "$TMP"/ccmorph-install.* 2>/dev/null` empty — set `TMPDIR=$d` for the run so the check is local).
7. Local mode is untouched: the existing cases 1–7 keep passing, and
   `zsh "$REPO/install.sh"` never calls the stub curl (assert `curl.argv` absent).

Each stub needs `#!/bin/sh` and `chmod +x`. Record argv with `printf '%s\n' "$@" >> "$CURL_LOG"`
where the log path is baked into the stub text at creation time (no env needed).

**Done when** the seven new assertions pass, the old ones still pass, `zsh -n install.sh`
is clean, and `bash install.sh` / `sh install.sh` from the worktree still install locally
(CI's install-check does this; run it by hand once against a throwaway `HOME`).

**Commit.** `feat(install): install from a download when run outside a source tree`

### A2. Windows is named, WSL is pointed at

**Why.** Git Bash/MSYS users get a confusing zsh error. Mainstream installers say
"use WSL" up front.

**Files.** `install.sh` (POSIX block, first thing in the remote/bootstrap section),
`tests/install_test.zsh`.

**Change.** At the very top of the POSIX code, before remote mode:

```sh
case "$(uname -s 2>/dev/null)" in
  MINGW*|MSYS*|CYGWIN*)
    echo "ccmorph runs on macOS and Linux, including WSL. On Windows, open a WSL shell and run this again there." >&2
    exit 1 ;;
esac
```

**Test first.** Case with a stub `uname` (`#!/bin/sh`, `echo MINGW64_NT-10.0`) first on
PATH, run via `sh "$REPO/install.sh"` → exit 1, stderr mentions `WSL`, nothing written
under `$HOME`. A second run without the stub still installs (reuse case 1's shape).

**Commit.** `feat(install): name Windows and point at WSL before anything else`

### A3. Dependency hints and offers for every package manager, not only brew

**Why.** On Linux the installer says "install zsh first" with three distro commands,
offers fzf only when `brew` exists, and says nothing useful about `claude`.

**Files.** `install.sh`, `tests/install_test.zsh`.

**Change.**
1. POSIX block (zsh missing): detect the manager and print **one** command:
   `brew install zsh`, `sudo apt-get install -y zsh`, `sudo dnf install -y zsh`,
   `sudo apk add zsh`, `sudo pacman -S zsh`, `sudo zypper install -y zsh`; fall back to
   the current three-line message when none is found. Never run it.
2. zsh section: add `_pkg_cmd() { … }` returning the install command for `$1` on the
   detected manager (same table), empty when unknown. Use it:
   - `fzf`: offer via `_offer $(_pkg_cmd fzf)` whenever `_pkg_cmd` is non-empty (today:
     brew only). Keep `|| true`.
   - `cliproxyapi`: brew only (no other manager ships it); otherwise print the
     upstream link as today.
   - `claude` missing (required): print, do not run:
     `Install Claude Code first: https://docs.anthropic.com/en/docs/claude-code/setup` and
     keep exiting 1. Do not embed an install command for another vendor's product.
   - `python3` too old: add the `_pkg_cmd python3` line to the existing message.
3. The detection order is brew, apt-get, dnf, apk, pacman, zypper — first found wins.
   `command -v` only; never `uname`-based guessing for managers.

**Tests first** (extend section 7 or add `# --- 9. package managers`):
- Fake bin with `apt-get` stub (`echo "STUB APT CALLED" >&2; exit 1`), no `brew`, no
  `fzf`, non-tty → exit 0, output contains `apt-get install -y fzf`, output does not
  contain `STUB APT CALLED`.
- Fake bin with `dnf` stub → output contains `dnf install -y fzf`.
- No `claude` in fake bin → exit 1, stderr/out names `claude` and contains
  `docs.anthropic.com`; nothing under `$HOME/.config/ccmorph`.
- zsh missing is covered at the POSIX level: run with `sh "$REPO/install.sh"` and a
  PATH whose only shells are `sh`; hard to stage reliably because the test itself runs
  zsh — instead unit-test the printed command by extracting the manager table into a
  function `_ccm_pkg_hint` in the POSIX block and calling
  `sh -c '. <(sed -n "/^_ccm_pkg_hint()/,/^}/p" install.sh); _ccm_pkg_hint zsh'` with an
  `apt-get` stub on PATH → prints `sudo apt-get install -y zsh`. (`sed -n` range on the
  function body keeps the test honest without running the whole bootstrap.)

**Commit.** `feat(install): offer optional deps through any package manager, name required ones`

### A4. PATH advice per shell

**Why.** The hint prints `export PATH=…` for everyone; fish users cannot paste it, and
nobody is told which file.

**Files.** `install.sh` (PATH section), `tests/install_test.zsh`.

**Change.** Keep the check (`":$PATH:" != *":$BIN_DIR:"*`). Choose the hint by
`${SHELL:t}`:

| shell | file named | line printed |
|---|---|---|
| `zsh` (or unknown) | `${ZDOTDIR:-$HOME}/.zshrc` | `export PATH="$BIN_DIR:$PATH"` |
| `bash` | `~/.bashrc` on Linux, `~/.bash_profile` on macOS (`uname -s` = Darwin) | same `export` |
| `fish` | `~/.config/fish/config.fish` | `fish_add_path $BIN_DIR` |

Say "Add this line to <file> yourself, then open a new terminal". Still no edit.

**Tests first.** Three runs of the fresh case with `SHELL=/usr/bin/fish`, `/bin/bash`,
`/bin/zsh` added to the `env -i` line (PATH already lacks `$HOME/.local/bin`): output
contains `fish_add_path`, `.bashrc` (on Linux; accept `.bash_profile` when `uname -s`
is Darwin), `.zshrc` respectively; `$HOME/.zshrc`, `$HOME/.bashrc` and
`$HOME/.config/fish/config.fish` do not exist afterwards.

**Commit.** `feat(install): PATH hint names the file and syntax for the user's shell`

### A5. Closing report: one next-step block, same for fresh and update

**Why.** The fresh-install footer lists four commands; after A1 the user may have no
tty and never saw the offers. Make the end of every run answer "did it work, what now".

**Files.** `install.sh` (footer), `tests/install_test.zsh`.

**Change.** After the existing Done line, always print:

```
Check:  ccmorph doctor
Next:   ccmorph add <name>      (fresh install only)
```

and, when `$BIN_DIR` is not on PATH, print the check as the absolute
`$BIN_DIR/ccmorph doctor` so it works before the shell is fixed. Remove the four-line
block if it duplicates this (keep `ccmorph add`, drop `use`/`list`: `add` now sets the
default, task B2).

**Test first.** Fresh case output contains `ccmorph doctor`; update case (section 6)
output contains `ccmorph doctor` and not `ccmorph add`.

**Commit.** `feat(install): end every run with the check and the next step`

---

## Phase B — first run inside the command

Independent of Phase A; B1–B3 touch different functions and can be done in any order.

### B1. `ccmorph run` with zero profiles points at `add`, not at `use`

**Why.** Today: `no default profile set — run: ccmorph use <profile> or ccmorph run -i`.
With no profiles, both suggestions fail.

**Files.** `lib/ccmorph-cli.zsh` (`run)` arm, the `if [[ -z "$_run_account" && -z "$active" ]]`
block), `tests/behaviour_coverage_test.py`.

**Change.** Before the existing message: if `"$dir"/*.conf(N)` is empty →
`_ccm_err "no profiles yet — run: ccmorph add <name>"` and `return 1`. Keep the
existing message for the "profiles exist, no default" case.

**Test first.** Add `test_run_with_no_profiles_points_at_add(box)`: no `seed`,
`box.run('run')` → returncode != 0, `'ccmorph add' in out.stderr`,
`'ccmorph use' not in out.stderr`, `box.log() == ''`. The existing
`test_run_with_no_default_and_no_profile_launches_nothing` seeds three profiles and must
keep passing unchanged.

**Commit.** `fix(cli): a run with no profiles says to add one, not to pick a default`

### B2. The first profile becomes the default

**Why.** `add` for `sub` writes `active`; `token`, `apikey` and `provider` do not, so a
new user's `ccmorph run` fails until they also learn `use`. Scope: only when **no**
default exists yet — an existing default is never changed by `add` (the `sub` arm's
unconditional write is pre-existing behaviour; leave it as is in this task).

**Files.** `lib/ccmorph-cli.zsh` (`add)` arm, just before `print -r -- "Next: ccmorph run $2"`),
`tests/behaviour_coverage_test.py`.

**Change.**
```zsh
if [[ ! -e "$dir/active" ]]; then
  _ccm_write "$dir/active" "$2" && print "${_ccm_dim}Default profile: $2 (first profile)${_ccm_reset}"
fi
```
`_ccm_write` failure here is not fatal to the add (the profile exists); do not `return 1`.

**Test first.** `test_the_first_added_profile_becomes_the_default(box)`:
`box.run('add', 't1', answer='token\nsk-ant-oat-canary\n')` → returncode 0 and
`box.active == 't1'`; then `box.run('add', 't2', answer='token\nsk-ant-oat-canary2\n')`
→ `box.active == 't1'`. (The `add` menu is read from stdin when there is no tty: the
first line answers the type prompt, the second the token — see `tests/cli_installed_test.py`
`setUpClass` for the proven answer shape.)

**Commit.** `fix(cli): the first profile added becomes the default`

### B3. `ccmorph doctor` with no profiles reports the environment

**Why.** The post-install check is `doctor`, but with no profile it prints one FAIL and
exits 1. Mainstream tools have a `doctor` that works on a fresh install.

**Files.** `lib/ccmorph-cli.zsh` (`_ccm_doctor`, the `if [[ -z "$name" ]]` branch),
`tests/doctor_test.py`.

**Change.** When `$name` is empty **and** `"$_CCMORPH_DIR"/*.conf(N)` is empty, print an
environment report through `_ccm_doctor_status` and return 0:

| status | line |
|---|---|
| OK | `ccmorph: <version> (<lib dir>)` |
| OK | `zsh: <$ZSH_VERSION>` |
| OK / FAIL | `python3: <x.y.z>` — FAIL below 3.8 with "need 3.8 or newer" |
| OK / FAIL | `claude executable: <path>` — reuse the existing whence logic |
| INFO | `fzf: <path>` or `fzf: not found (numbered menus)` |
| INFO | `cliproxyapi: <path>` or `cliproxyapi: not found (Claude-family models only on gateways)` |
| OK / INFO | `PATH: <bin dir of this ccmorph> is on PATH` / `… not on PATH` |
| INFO | `state: <$_CCMORPH_DIR> (no profiles yet) — next: ccmorph add <name>` |

Return 1 only when a FAIL row was printed. When profiles exist but none is the default,
keep the current FAIL line but append `profiles: a b c` via `_ccm_accounts_hint`.
Doctor stays read-only: no mkdir, no chmod, no network.

**Tests first** (`tests/doctor_test.py`, follow `run_doctor`'s env shape):
- empty state dir, stub `claude` on PATH → exit 0, output has `no profiles yet`,
  `python3:`, `claude executable:`; state dir still has no `profiles/` dir afterwards.
- same without `claude` on PATH → exit 1, `claude executable: not found`.
- two confs, no `active` → exit 1 and output lists both names.

**Commit.** `feat(doctor): report the environment on a fresh install`

---

## Phase C — docs, CI, and the optional self-update

### C1. README, guide, CHANGELOG

**Files.** `README.md` (Install, Quick start), `docs/guide.md` (Install details,
Updating), `CHANGELOG.md` (`## Unreleased`).

**Change.**
- README Install: lead with the one-liner (decision 2), keep the clone form as "or from
  a clone". Mention `CCMORPH_REF` in one sentence. Quick start: `ccmorph doctor`, then
  `ccmorph add work`, then `ccmorph run` (drop `use` from the first four lines; mention it
  in Usage).
- Guide: the same, plus the package-manager table (what is offered, what is only
  named), the per-shell PATH note, WSL.
- CHANGELOG `## Unreleased`: bullets under the **existing** `### Added` and `### Changed`
  headings. `test_unreleased_has_one_changed_heading` fails on a second `### Changed`,
  and `test_unreleased_offers_the_formula_only_where_it_is_removed` fails on the word
  "formula" outside `### Removed` — do not write it.

**Test.** `CCMORPH_LIB_DIR="$PWD/lib" uv run --with pytest python -m pytest tests/docs_claims_test.py tests/docs_anchor_test.py -q`.
Also grep the README for `./install.sh` and make sure every remaining mention is still true.

**Commit.** `docs: one-line install, environment check, first-profile default`

### C2. CI exercises the remote path with a local tarball

**Files.** `.github/workflows/ci.yml` (install-check job).

**Change.** After the existing non-interactive install step, add:

```yaml
- name: one-line install from a local tarball (remote mode, real curl and tar)
  run: |
    ./install.sh --uninstall
    rm -rf "$HOME/.config/ccmorph"
    mkdir -p /tmp/stage && git archive --format=tar --prefix=ccmorph-main/ HEAD | gzip > /tmp/stage/src.tar.gz
    CCMORPH_INSTALL_URL="file:///tmp/stage/src.tar.gz" sh -c "$(cat install.sh)" </dev/null
    test -r "$HOME/.config/ccmorph/lib/ccmorph.sh"
    sh -c 'ccmorph --version'
```

`curl` supports `file://`; this proves the real fetch/unpack/re-exec chain on both
runners without touching GitHub during CI. Keep `</dev/null`.

**Test.** Run the same lines locally against a throwaway `HOME` (set `HOME=$(mktemp -d)`
and put the `claude` stub on PATH as the job does).

**Commit.** `ci: install through remote mode from a local tarball`

### C3 (optional, proposed separately). `ccmorph update`

Only after Alex agrees. Shape: add `install.sh` to `COMPONENTS` (and to the manifest
`want=` in the install test); `ccmorph update [ref]` runs
`CCMORPH_REF=${1:-main} sh "$_CCMORPH_LIB_DIR/../install.sh" --remote`; `_ccm_usage`
gains the verb under "More commands". Tests: a stub curl serving a tarball whose
`lib/ccmorph.sh` carries a different `_CCMORPH_VERSION`, then `ccmorph --version` reports
it; profiles survive (reuse section 6's user-state checks). Commit
`feat(cli): ccmorph update re-runs the installer from a download`.

---

## Order, dependencies, and the PR

```
A1 → A2 → A3 → A4 → A5        (same file; serial)
B1, B2, B3                      (independent; any time)
C1 after A5 and B3; C2 after A1; C3 last, only if approved
```

Suggested session split for a Sonnet-class implementer: one session per phase-letter
group (A1–A2, A3–A5, B1–B3, C1–C2), each ending with the four suites green and a push
to `claude/festive-maxwell-dj0f8e` (`git push -u origin claude/festive-maxwell-dj0f8e`).

PR title: `proposed: one-line install and first-run experience`. Body: link this plan,
list the decisions above, name what is out of scope, paste the four suite results from
the exact head, and call out that no launch, picker or backend behaviour changed (so no
high-risk class applies; Alex's ✅ is for the product decision, not a security review).
Remove `docs/plans/install-experience.md` in the last commit before merge, or move its
content to the vault (`20 Projects/ccmorph/Install experience — Plan — 2026-10-27.md`).
