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
# reports rather than arranging behind your back. A managed block left by an
# older version is reported, never removed: the file is the user's.

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
# `--remote` forces this from inside a tree; `ccmorph update` runs the installed copy with it.
# POSIX-only syntax in this block, and it runs first: no zsh yet, no `set -eu`.
_ccm_local=0
case "$0" in
  */install.sh|install.sh)
    [ -r "$0" ] && [ -r "$(dirname "$0")/lib/ccmorph.sh" ] && _ccm_local=1 ;;
esac
if [ "$_ccm_local" = 0 ] || [ "${1:-}" = "--remote" ]; then
  [ "${1:-}" = "--remote" ] && shift
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
# guess from the OS), or nothing. It only PRINTS: zsh and claude are named,
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
# install.sh ships too (an executable at the install root), so `ccmorph update` and
# `ccmorph uninstall` have the installer to run without a clone or a URL.
EXECUTABLES=(bin/ccmorph install.sh)
DATA_FILES=(share/model-catalogue.json share/window-sources.json share/provider-presets.tsv
  share/claude-plugin/.claude-plugin/plugin.json share/claude-plugin/commands/hop.md)
COMPONENTS=($LIBS $EXECUTABLES $DATA_FILES)

# ---- uninstall ------------------------------------------------------------
# Must run before the UI module is sourced: a standalone install.sh (no sibling
# ccmorph-ui.zsh) must still be able to remove the command.
#
# Only the command goes; nothing else in ~/.local/bin is ours to touch. Everything
# else ccmorph wrote is the user's — the state root (profiles, credentials and
# each profile's session history), the usage receipts, and on macOS the keychain
# items the confs point at — so it is NAMED, with the command that removes it,
# rather than deleted here or left for them to discover later.
if [[ "${1:-}" == "--uninstall" ]]; then
  # Claude Code records a plugin installed from a profile session under that
  # profile's spelling of the shared ~/.claude/plugins store. Every ccmorph run
  # repairs that, but there is no run after this one: repair now, so the plugin
  # still loads in native claude once the state root is gone. Best effort — a
  # missing or older core (no such subcommand) must not stop the uninstall.
  if [[ -x "$CONFIG_DIR/libexec/ccmorph-core" ]]; then
    "$CONFIG_DIR/libexec/ccmorph-core" util normalize-plugin-index "$HOME" "$CONFIG_DIR/profiles" >/dev/null 2>&1 || :
  fi
  rm -f "$BIN_DIR/ccmorph"
  print -r -- "Removed $BIN_DIR/ccmorph."
  _un_state="${XDG_STATE_HOME:-$HOME/.local/state}/ccmorph"
  _un_profiles=("$CONFIG_DIR"/*.conf(N))
  _un_rm=("$CONFIG_DIR")
  [[ -d "$_un_state" ]] && _un_rm+=("$_un_state")
  # The keychain items are read off the confs, never by scanning the keychain
  # (which prompts): a conf is the only thing that names them. `|| :` because a
  # grep with no match exits 1, an assignment takes its substitution's status,
  # and `set -e` would end the uninstall right there, mid-report.
  _un_keychain=()
  if [[ "$(uname -s 2>/dev/null)" == Darwin ]]; then
    _un_keychain=(${(f)"$(command grep -hsE '^(api_key|token)=keychain:.' "$CONFIG_DIR"/*.conf(N) /dev/null || :)"})
    _un_keychain=(${_un_keychain#*=keychain:})
    _un_keychain=(${_un_keychain:#})
  fi
  print -r -- "Kept, because they are yours:"
  print -r -- "  $CONFIG_DIR  (${#_un_profiles} profiles, their credentials and session history)"
  [[ -d "$_un_state" ]] && print -r -- "  $_un_state  (usage receipts)"
  (( ${#_un_keychain} )) && print -r -- "  login keychain  (${#_un_keychain} ccmorph item(s))"
  print -r -- "To remove those too:"
  print -r -- "  rm -rf" "${(q-)_un_rm[@]}"
  for _un_ref in "${_un_keychain[@]}"; do
    print -r -- "  security delete-generic-password -s ccmorph -a ${(q-)_un_ref}"
  done
  print -r -- "Plugins installed from a profile live in $HOME/.claude/plugins and keep working in claude."
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
  (( ${missing[(I)claude*]} )) && print "Install Claude Code first: https://docs.anthropic.com/en/docs/claude-code/setup"
  print "Install those first, then re-run."
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
  mkdir -p "$CONFIG_DIR"/{bin,lib,libexec,share/claude-plugin/{.claude-plugin,commands}}
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
# ccmorph-core is compiled Go, so it is the component that is not a tracked
# file. A release tarball carries one prebuilt binary per
# platform (<name>-<os>-<arch>, from `make dist`); a checkout carries the ones
# `make shim` built. Nothing is compiled here: an installer that needs a Go
# toolchain on a user's machine is a second dependency for one helper.
_shim_os="${$(uname -s):l}"
_shim_arch="$(uname -m)"
case "$_shim_arch" in x86_64) _shim_arch=amd64 ;; aarch64) _shim_arch=arm64 ;; esac
for _name in ccmorph-core; do
  _shim=""
  for _f in "$SRC/libexec/$_name-$_shim_os-$_shim_arch" "$SRC/libexec/$_name"; do
    [[ -f "$_f" && -x "$_f" ]] && { _shim="$_f"; break }
  done
  if [[ -z "$_shim" ]]; then
    c_r "No $_name binary for $_shim_os-$_shim_arch beside this installer."
    c_dim "  From a checkout: run 'make shim' (needs Go), then re-run ./install.sh."
    c_dim "  Otherwise install a release: it ships the binary for your platform."
    exit 1
  fi
  # Remove first: copying over a running-signed Mach-O in place keeps the kernel's stale
  # code-signature cache for the old bytes, and macOS then kills the new binary (exit 137).
  [[ "${_shim:A}" == "${CONFIG_REAL}/libexec/$_name" ]] \
    || { rm -f "$CONFIG_DIR/libexec/$_name" && cp "$_shim" "$CONFIG_DIR/libexec/$_name"; }
  chmod +x "$CONFIG_DIR/libexec/$_name"
done
chmod +x "$BIN_DIR/ccmorph"
# Move profile homes through the same guarded owner used by normal launches.
CCMORPH_STATE_DIR="$CONFIG_DIR" CCMORPH_LIB_DIR="$CONFIG_DIR/lib" zsh -fc \
  'source "$CCMORPH_LIB_DIR/ccmorph.sh"; _ccm_secure_dirs' || exit 1
# Consolidate legacy context state only after the owning tool is installed.
CCMORPH_STATE_DIR="$CONFIG_DIR" "$CONFIG_DIR/libexec/ccmorph-core" model-window --migrate
# Three of the four Python helpers (v0.3.0) are now `ccmorph-core` subcommands
# and nothing running names them, so an upgrade clears that dead weight.
# ccmorph-adapter is the exception: a session launched before the Go port keeps
# naming it in its route spec for a mid-session /model (the shim execs it for
# --ensure-config), and deleting the file under that session breaks the switch
# with a message about gateway availability that names neither the update nor
# the restart (#52). The old copy costs only disk now that lib/ no longer calls
# it, and no installer ever writes the path again, so it can simply stay.
rm -f "$CONFIG_DIR"/libexec/{model-window,settings-compose,refresh-model-registry,gateway-shim}
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
