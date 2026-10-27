#!/usr/bin/env zsh
# ccmorph one-click installer.
#
# Ships the INFRASTRUCTURE only — never any profile or token. Profiles
# (*.conf), runtime state, and credentials are gitignored and are created by
# the user after install via `ccmorph add`. A fresh install therefore starts
# empty, like a newly-installed app.
#
# Run:  ./install.sh          (from a clone/download of the ccmorph repo)
# Idempotent: re-running updates files in place.
# Uninstall: ./install.sh --uninstall
#
# This installer does NOT edit your shell. ccmorph is one executable; the only
# requirement is that its directory be on your PATH, which this script checks and
# reports rather than arranging behind your back. It does REMOVE a managed block
# left by an older version, because that block sourced a library that no longer
# defines any command and would fail silently in every new shell.

# Windows shells (Git Bash, MSYS, Cygwin) have no zsh to re-exec into, and what
# they used to hit was an error about zsh, not about Windows. Name the platform
# and the way forward first, before the download and before anything is written.
case "$(uname -s 2>/dev/null)" in
  MINGW*|MSYS*|CYGWIN*)
    echo "ccmorph runs on macOS and Linux, including WSL. On Windows, open a WSL shell and run this again there." >&2
    exit 1 ;;
esac

# Remote mode. `sh -c "$(curl -fsSL .../install.sh)"` and `curl ... | sh` run this
# script with $0 = sh and no repo beside it, so everything below that reads the
# tree (the copies, the manifest) would fail on a path that does not exist. Fetch
# the tree and hand over to ITS installer instead: the downloaded copy has
# lib/ccmorph.sh beside it, takes the local path below, and never comes back here.
# POSIX-only syntax in this block, and it runs first: no zsh yet, no `set -eu`.
_ccm_local=0
case "$0" in
  */install.sh|install.sh)
    [ -r "$0" ] && [ -r "$(dirname "$0")/lib/ccmorph.sh" ] && _ccm_local=1 ;;
esac
if [ "$_ccm_local" = 0 ]; then
  # The default is the latest RELEASE tarball, not main: main moves with every
  # merge, and a one-liner pointed at it hands a brand-new user code no release
  # has vetted. GitHub resolves releases/latest itself, so nothing here has to
  # track a version. CCMORPH_REF opts into the GitHub archive of a branch or tag
  # instead (CCMORPH_REF=main for unreleased code), and CCMORPH_INSTALL_URL
  # replaces the whole URL either way.
  if [ -n "${CCMORPH_REF:-}" ]; then
    _ccm_url="${CCMORPH_INSTALL_URL:-https://github.com/akleizor/ccmorph/archive/${CCMORPH_REF}.tar.gz}"
  else
    _ccm_url="${CCMORPH_INSTALL_URL:-https://github.com/akleizor/ccmorph/releases/latest/download/ccmorph.tar.gz}"
  fi
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

# The install command for package $1 on the first package manager found (brew,
# apt-get, dnf, apk, pacman, zypper, in that order; `command -v` only, never a
# guess from the OS), or nothing. It only PRINTS: zsh, python3 and claude are named,
# never installed. POSIX sh, because the zsh-missing message below needs it before
# any zsh exists; the zsh half of this file calls it too, so there is one table.
_ccm_pkg_hint() {
  if command -v brew >/dev/null 2>&1; then echo "brew install $1"
  elif command -v apt-get >/dev/null 2>&1; then echo "sudo apt-get install -y $1"
  elif command -v dnf >/dev/null 2>&1; then echo "sudo dnf install -y $1"
  elif command -v apk >/dev/null 2>&1; then echo "sudo apk add $1"
  elif command -v pacman >/dev/null 2>&1; then echo "sudo pacman -S $1"
  elif command -v zypper >/dev/null 2>&1; then echo "sudo zypper install -y $1"
  fi
}

# Re-exec under zsh when started by another shell — `bash install.sh` or `sh
# install.sh`; a curl-pipe never gets here, remote mode handed it to a downloaded
# copy above. Everything below this point is zsh: the next few
# lines alone use ${0:A:h} and glob qualifiers, and bash reads ${0:A:h} as a
# substring expansion whose offset it evaluates arithmetically, dying with
# "A: unbound variable" — a message about a variable that does not exist, printed
# before the dependency checks that would have named the real requirement.
# POSIX-only syntax in this block: it runs under whatever shell was used.
if [ -z "${ZSH_VERSION:-}" ]; then
  if command -v zsh >/dev/null 2>&1; then
    exec zsh "$0" "$@"
  fi
  echo "ccmorph requires zsh, and no zsh was found on PATH." >&2
  _ccm_hint="$(_ccm_pkg_hint zsh)"
  if [ -n "$_ccm_hint" ]; then
    echo "  Install it first, then re-run:" >&2
    echo "  $_ccm_hint" >&2
  else
    echo "  macOS: zsh ships with the system (/bin/zsh)." >&2
    echo "  Linux: install it first — apt install zsh, dnf install zsh, apk add zsh." >&2
  fi
  exit 1
fi

set -eu

# This installer keeps code and state in ONE directory, which is right for a
# clone-install: an upgrade replaces the components and leaves everything else
# alone, and ccmorph.sh derives its own library path, so it finds its helpers
# beside it either way. A package manager separates them instead — the library
# lands in a read-only prefix and only this path stays writable. Both work
# because the runtime no longer assumes they are the same place.
# XDG_CONFIG_HOME is honoured here exactly as ccmorph.sh honours it, or the two
# would disagree about where profiles live on a machine that sets it.
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/ccmorph"
BIN_DIR="$HOME/.local/bin"
SRC="${0:A:h}"                       # dir this script lives in (the repo root)

# A module is any ccmorph-*.zsh beside ccmorph.sh. Enumerated rather than
# listed, so a new module ships with no edit here (only ccmorph.sh's own loader
# block has to name it). (N) keeps a standalone install.sh — which has no module
# beside it — working.
MODULES=("$SRC"/lib/ccmorph-*.zsh(N:t))

# Every tracked runtime component the launcher depends on — one list; data
# files (no exec bit) are subtracted to derive EXECUTABLES, so a new component
# is added exactly once. Modules are data: they are sourced, never run.
# Paths are relative to the repo root and installed with the same structure.
LIBS=(lib/ccmorph.sh ${MODULES/#/lib/})
EXECUTABLES=(bin/ccmorph libexec/ccmorph-adapter libexec/gateway-shim libexec/model-window libexec/settings-compose libexec/refresh-model-registry)
DATA_FILES=(share/model-catalogue.json share/window-sources.json)
COMPONENTS=($LIBS $EXECUTABLES $DATA_FILES)

# ---- uninstall ------------------------------------------------------------
# Must run before the UI module is sourced: a standalone install.sh (no sibling
# ccmorph-ui.zsh) must still be able to remove the command.
if [[ "${1:-}" == "--uninstall" ]]; then
  rm -f "$BIN_DIR/ccmorph" "$BIN_DIR/ccrelay"
  print -r -- "Left $CONFIG_DIR in place (it holds your profiles). Remove it by hand if you truly want them gone."
  print -r -- "Open a new shell to finish."
  exit 0
fi

source "$SRC/lib/ccmorph-ui.zsh" 2>/dev/null || true

c_ok() { print -r -- "${_ccm_accent:-}$*${_ccm_reset:-}"; }
c_y()  { print -r -- "${_ccm_yellow:-}$*${_ccm_reset:-}"; }
c_r()  { print -r -- "${_ccm_red:-}$*${_ccm_reset:-}"; }
c_dim(){ print -r -- "${_ccm_gray:-}$*${_ccm_reset:-}"; }

# ---- prior state, read BEFORE anything is written -------------------------
# An update must be able to SAY it is an update. Every fact below describes the
# machine as it was on arrival, so the closing report can name what was
# preserved — a re-run that prints "fresh install" over someone's eight live
# profiles reads as data loss and teaches them never to update again.
PRIOR_VERSION=""
if [[ -r "$CONFIG_DIR/lib/ccmorph.sh" ]]; then
  # Grep the assignment rather than sourcing: the installed copy may be older
  # or newer than this one, and sourcing it here would run its whole init.
  PRIOR_VERSION="$(grep -m1 '^_CCMORPH_VERSION=' "$CONFIG_DIR/lib/ccmorph.sh" 2>/dev/null)"
  PRIOR_VERSION="${${PRIOR_VERSION#*=}//\"/}"
fi
NEW_VERSION="$(grep -m1 '^_CCMORPH_VERSION=' "$SRC/lib/ccmorph.sh" 2>/dev/null)"
NEW_VERSION="${${NEW_VERSION#*=}//\"/}"

# Profiles are the thing a user fears losing, so they are counted, not guessed.
# (N) so an empty or absent dir yields zero matches instead of a literal glob.
PRIOR_PROFILES=("$CONFIG_DIR"/*.conf(N))
IS_UPDATE=0
[[ -n "$PRIOR_VERSION" || ${#PRIOR_PROFILES} -gt 0 ]] && IS_UPDATE=1

if (( IS_UPDATE )); then
  if [[ -n "$PRIOR_VERSION" && "$PRIOR_VERSION" != "$NEW_VERSION" ]]; then
    c_dim "Updating ccmorph ${PRIOR_VERSION} -> ${NEW_VERSION}"
  else
    c_dim "Re-running ccmorph ${NEW_VERSION} installer (update in place)"
  fi
fi

# ---- dependency checks ----------------------------------------------------
missing=()
command -v zsh     >/dev/null 2>&1 || missing+=("zsh")
command -v python3 >/dev/null 2>&1 || missing+=("python3 (3.8 or newer)")
command -v claude  >/dev/null 2>&1 || missing+=("claude (Claude Code CLI)")
# The install command for tool $1 on the detected package manager, split into
# words in $reply (empty when no manager is known), so it can be printed or handed
# to _offer, which runs "$@". The table itself lives in _ccm_pkg_hint at the top.
_pkg_cmd() { reply=(${=$(_ccm_pkg_hint "$1")}) }

if (( ${#missing} )); then
  c_r "Missing required dependencies:"
  for m in "${missing[@]}"; do print "  - $m"; done
  # Required tools are named, never installed: they need root or belong to another
  # vendor. Claude Code gets its setup page and no command at all.
  _pkg_cmd zsh;     (( ${missing[(I)zsh]} ))     && (( ${#reply} )) && print "  zsh:     ${reply[*]}"
  _pkg_cmd python3; (( ${missing[(I)python3*]} )) && (( ${#reply} )) && print "  python3: ${reply[*]}"
  (( ${missing[(I)claude*]} )) && print "Install Claude Code first: https://docs.anthropic.com/en/docs/claude-code/setup"
  print "Install those first, then re-run."
  exit 1
fi

# A python3 that EXISTS is not necessarily one that can run the helpers. The
# floor is 3.8, set by the walrus in gateway-shim. Checking it here matters more
# than it looks: gateway-shim is spawned only for upstreams that recycle tool-use
# ids, so a too-old interpreter leaves an install that appears to work — for days
# — and then dies inside a child process with a bare SyntaxError in a log, miles
# from the cause. Fail here, naming the version, while the user is still looking.
if ! python3 -c 'import sys; sys.exit(0 if sys.version_info >= (3, 8) else 1)' 2>/dev/null; then
  _pyver="$(python3 -c 'import sys; print(".".join(map(str, sys.version_info[:3])))' 2>/dev/null || print unknown)"
  c_r "python3 is too old: found ${_pyver}, need 3.8 or newer."
  c_dim "  ccmorph's helpers are stdlib-only but use 3.8 syntax (gateway-shim)."
  c_dim "  macOS: brew install python3.  Linux: your distro's python3.8+ package."
  _pkg_cmd python3
  (( ${#reply} )) && c_dim "  With your package manager: ${reply[*]}"
  exit 1
fi

# Optional dependencies are OFFERED, never installed silently. Both are real
# capability gates that a first run otherwise discovers as a mysterious failure
# hours later: no translator means most of a multi-vendor gateway is
# unreachable, and no fzf means the choosers do not exist. Offering keeps the
# decision with the operator — an installer that pulls a compiled binary onto a
# managed laptop unannounced is the kind of surprise that gets a tool banned.
#
# Non-interactive (no tty) always declines and says so:
# a piped or CI run must never block on a prompt, and must never install either.
# NOTE for callers: this returns non-zero on a decline or a failed install, and
# install.sh runs `set -e` — so EVERY call site must append `|| true`. A declined
# optional dependency is a normal outcome; aborting the install over it would
# break precisely the first-run case this offer exists to improve.
_offer() {   # $1 = human reason block (already printed), $2.. = command
  if [[ ! -t 0 || ! -t 2 ]]; then
    c_dim "  non-interactive — skipped. Run yourself: $*"
    return 1
  fi
  if (( $+functions[ccm_ui_confirm] )); then
    ccm_ui_confirm "Install now? $*" || { c_dim "  skipped. Run yourself when ready: $*"; return 1 }
  else
    # The UI module was not beside install.sh — fall back to a plain read.
    local answer
    print -n "Install now? $* [y/N] "
    read -r answer
    [[ "${answer:l}" == (y|yes) ]] || { c_dim "  skipped. Run yourself when ready: $*"; return 1 }
  fi
  "$@" || { c_r "  '$*' failed — install it by hand, then re-run this installer."; return 1 }
  return 0
}

# The dialect translator. `adapter=true` profiles front a gateway whose
# non-Anthropic models speak OpenAI dialects Claude Code cannot; ccmorph-adapter
# shells out to this to translate. Claude-family models need none of it, which is
# why this is a warning and not a hard requirement — and why the note says which
# half of the catalogue is at stake instead of just naming a missing binary.
# homebrew/core, upstream github.com/router-for-me/CLIProxyAPI, no runtime deps.
if ! command -v cliproxyapi >/dev/null 2>&1; then
  c_y "note: cliproxyapi (dialect translator) not found."
  c_dim "  Without it, an 'adapter=true' profile can reach only its Claude-family"
  c_dim "  models; the gateway's OpenAI-shaped ones (gpt-*, kimi, glm, qwen) fail"
  c_dim "  at launch. Claude models work either way."
  if command -v brew >/dev/null 2>&1; then
    _offer brew install cliproxyapi || true
  else
    c_dim "  No brew on PATH — see https://github.com/router-for-me/CLIProxyAPI"
  fi
fi

if ! command -v fzf >/dev/null 2>&1; then
  c_y "note: fzf not found — interactive pickers will use numbered menus."
  c_dim "  Install fzf for searchable lists; explicit command-line values also work."
  # Offered through whichever package manager is there, not only brew.
  _pkg_cmd fzf
  if (( ${#reply} )); then _offer "${reply[@]}" || true; fi
fi

# ---- copy infrastructure --------------------------------------------------
mkdir -p "$CONFIG_DIR" "$BIN_DIR"

# "Already home" is a question about DIRECTORIES, not strings. SRC is resolved
# by :A, so CONFIG_DIR must be resolved too — otherwise /tmp vs /private/tmp (a
# symlink on macOS), or any symlinked $HOME, compares unequal while naming one
# directory. Every copy then becomes a copy onto itself, which cp refuses, and
# `set -e` turns that into an install that aborted before wiring the rc. Resolved
# after mkdir so the path exists, and only for this comparison: the literal
# CONFIG_DIR is what gets written into the rc, where a readable ~ path matters.
CONFIG_REAL="${CONFIG_DIR:A}"

# If running from inside the target dir, files are already home; otherwise copy.
if [[ "$SRC" != "$CONFIG_REAL" ]]; then
  mkdir -p "$CONFIG_DIR"/{bin,lib,libexec,share}
  for _f in "${COMPONENTS[@]}"; do cp "$SRC/$_f" "$CONFIG_DIR/$_f"; done
fi
cp "$SRC/bin/ccmorph" "$BIN_DIR/ccmorph"
# The opt-in reference, so a host can see every seam without reading the source.
# Copied, never activated: local.sh itself stays absent until someone writes it.
# Same directory guard as above: in-place, this is a copy onto itself.
if [[ "$SRC" != "$CONFIG_REAL" ]]; then
  cp "$SRC/share/local.sh.example" "$CONFIG_DIR/local.sh.example"
  cp "$SRC/share/profile.conf.example" "$CONFIG_DIR/profile.conf.example"
fi
chmod +x "${EXECUTABLES[@]/#/$CONFIG_DIR/}"
chmod +x "$BIN_DIR/ccmorph"
# Move profile homes through the same guarded owner used by normal launches.
CCMORPH_STATE_DIR="$CONFIG_DIR" CCMORPH_LIB_DIR="$CONFIG_DIR/lib" zsh -fc \
  'source "$CCMORPH_LIB_DIR/ccmorph.sh"; _ccm_secure_dirs' || exit 1
# Consolidate legacy context state only after the owning tool is installed.
CCMORPH_STATE_DIR="$CONFIG_DIR" "$CONFIG_DIR/libexec/model-window" --migrate
# The retired wrapper: `ccrelay` is now `ccmorph hop`. Removed rather than left
# behind, because the old script sources a library that no longer defines it and
# would answer with a load failure instead of saying what replaced it.
rm -f "$BIN_DIR/ccrelay"
c_ok "Installed infrastructure to $CONFIG_DIR and the ccmorph command to $BIN_DIR"

# Startup files are user-owned. Older releases may have left a managed block;
# report it without editing the file so installation is byte-for-byte safe.
if [[ -f "${ZDOTDIR:-$HOME}/.zshrc" ]] && grep -qF '# >>> ccmorph >>>' "${ZDOTDIR:-$HOME}/.zshrc"; then
  c_y "Found a legacy ccmorph block in ${ZDOTDIR:-$HOME}/.zshrc; remove it by hand if desired."
fi

# ---- PATH ------------------------------------------------------------------
# Checked and reported, never arranged: putting a directory on someone's PATH is
# an edit to their shell config, and this tool does not make those. ~/.local/bin is
# already on PATH on most machines, so for most people there is nothing to do.
# The hint names the file and the syntax for the shell this was started from
# ($SHELL is the login shell, which is the one whose startup file matters). Bash
# reads ~/.bash_profile for a login shell on macOS and ~/.bashrc elsewhere.
CCMORPH_CMD=ccmorph   # how the closing report spells the command: bare once it is on PATH
if [[ ":$PATH:" != *":$BIN_DIR:"* ]]; then
  CCMORPH_CMD="$BIN_DIR/ccmorph"   # absolute, so the check works before the shell is fixed
  _login_shell="${SHELL:-}"   # unset under cron or `env -i`; set -u must not kill the hint
  case "${_login_shell:t}" in
    fish) _rc_file="$HOME/.config/fish/config.fish"; _rc_line="fish_add_path $BIN_DIR" ;;
    bash) if [[ "$(uname -s 2>/dev/null)" == Darwin ]]; then _rc_file="$HOME/.bash_profile"; else _rc_file="$HOME/.bashrc"; fi
          _rc_line="export PATH=\"$BIN_DIR:\$PATH\"" ;;
    *)    _rc_file="${ZDOTDIR:-$HOME}/.zshrc"; _rc_line="export PATH=\"$BIN_DIR:\$PATH\"" ;;
  esac
  print ""
  c_y "$BIN_DIR is not on your PATH, so 'ccmorph' will not be found yet."
  c_dim "  Add this line to $_rc_file yourself, then open a new terminal:"
  print "  $_rc_line"
fi

print ""
if (( IS_UPDATE )); then
  # Name what SURVIVED, explicitly. These files are never in COMPONENTS and are
  # gitignored, so they were always safe — but "always safe" is invisible, and an
  # update that looks destructive is an update nobody runs twice.
  if [[ -n "$PRIOR_VERSION" && "$PRIOR_VERSION" != "$NEW_VERSION" ]]; then
    c_ok "Done. Updated ccmorph ${PRIOR_VERSION} -> ${NEW_VERSION}."
  else
    c_ok "Done. ccmorph ${NEW_VERSION} reinstalled in place."
  fi
  kept=()
  (( ${#PRIOR_PROFILES} )) && kept+=("${#PRIOR_PROFILES} profile(s)")

  [[ -f "$CONFIG_DIR/local.sh" ]]      && kept+=("local.sh overrides")
  if (( ${#kept} )); then
    c_dim "Preserved: ${(j:, :)kept}"
  fi
  c_dim "The new code is live immediately — ccmorph reads it on each run."
else
  c_ok "Done. ccmorph is installed (no profiles — fresh install)."
fi

# One closing block for every run, so the end of the output answers "did it work,
# what now". Only a fresh install has no profile to add yet; an update has its own.
print "Check:  $CCMORPH_CMD doctor"
(( IS_UPDATE )) || print "Next:   ccmorph add <name>"

# Context discovery is metadata-only and automatic on add/unknown launch.
# Installing must never start an inference sweep.
