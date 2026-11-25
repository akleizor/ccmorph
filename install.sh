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
  # The download must carry the core binary its own installer will place. A
  # GitHub tag archive carries only libexec/.gitkeep, and a tag's installer may
  # be an older release that copies lib/ before it looks for the binary — the
  # failure then leaves a new library beside the old core (#721). Validated
  # HERE, before that installer runs, so a failed update changes nothing. A
  # release tarball names the binary per platform (<name>-<os>-<arch>, from
  # `make dist`); a checkout's archive carries the plain one.
  # Only an install needs it: `--uninstall` places nothing, so a standalone
  # install.sh --uninstall still hands a coreless tag archive to its own
  # installer, which removes the command without a core.
  if [ "${1:-}" != "--uninstall" ]; then
    _ccm_os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    _ccm_arch="$(uname -m)"
    case "$_ccm_arch" in
      x86_64) _ccm_arch=amd64 ;;
      aarch64) _ccm_arch=arm64 ;;
    esac
    _ccm_core=0
    for _ccm_f in "${_ccm_src}libexec/ccmorph-core-${_ccm_os}-${_ccm_arch}" "${_ccm_src}libexec/ccmorph-core"; do
      if [ -f "$_ccm_f" ] && [ -x "$_ccm_f" ]; then _ccm_core=1; break; fi
    done
    [ "$_ccm_core" = 1 ] || _ccm_fail "No ccmorph-core binary for ${_ccm_os}-${_ccm_arch} in the download: $_ccm_url — nothing was changed; install a release, which ships the binary for your platform."
  fi
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

# From here on this file is zsh, and it runs under the options it was written
# for. zsh reads the user's ~/.zshenv before the first line of any script, and
# options set there change this file's own syntax: `unsetopt bareglobqual` made
# the (N:t) module glob below fatal, and `setopt ignorebraces` broke the
# layout's brace list and every `{ ...; break }` (#778). The same reset as
# bin/ccmorph and lib/ccmorph-launch.zsh:15-18; the setopt lines name what this
# file relies on.
emulate -L zsh
setopt bare_glob_qual
unsetopt ignore_braces ignore_close_braces extended_glob
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
# `ccmorph uninstall` have the installer to run without a clone or a URL. The usage
# exporter ships in share/ — the README's export step runs it from there, so every
# install route places it, not only a checkout (#261).
EXECUTABLES=(bin/ccmorph install.sh share/usage-export)
DATA_FILES=(share/model-catalogue.json share/window-sources.json share/provider-presets.tsv
  share/claude-plugin/.claude-plugin/plugin.json
  share/claude-plugin/commands/hop.md share/claude-plugin/commands/profile.md
  share/claude-plugin/hooks/hooks.json
  share/claude-plugin-router/.claude-plugin/plugin.json share/claude-plugin-router/.mcp.json
  share/claude-plugin-router/hooks/hooks.json share/claude-plugin-router/skills/router/SKILL.md
  share/zsh/site-functions/_ccmorph)
COMPONENTS=($LIBS $EXECUTABLES $DATA_FILES)

# ---- uninstall ------------------------------------------------------------
# A standalone install.sh (no library beside it) must still be able to remove the command.
#
# Only the command goes; nothing else in ~/.local/bin is ours to touch. Everything
# else ccmorph wrote is the user's — the state root (profiles, credentials and
# each profile's session history), the usage receipts, and on macOS the keychain
# items the confs point at — so it is NAMED, with the command that removes it,
# rather than deleted here or left for them to discover later.
if [[ "${1:-}" == "--uninstall" ]]; then
  # The state root the RUNTIME uses, derived exactly as lib/ccmorph.sh derives
  # it: CCMORPH_STATE_DIR, else XDG_CONFIG_HOME/ccmorph, else ~/.config/ccmorph.
  # On a host that sets the override, the profiles, credentials and session
  # history live under the redirected root while CONFIG_DIR holds only the
  # installed code — a report that named CONFIG_DIR as the profiles' home would
  # name a directory nothing lives in and never the real one (#143) — unless the
  # override came after they already lived there: a later redirect moves nothing
  # (docs: Where state lives), so CONFIG_DIR holds the code AND those profiles,
  # and they stay user data — counted and named beside the code, never folded
  # into it (#165).
  _un_root="${CCMORPH_STATE_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/ccmorph}"
  # The CODE root is wherever the install actually put it, and the launcher
  # finds it through the record the install left beside the command (#834) —
  # so the uninstall resolves the same record here, BEFORE that record leaves
  # with the command below: an install made under an exported XDG_CONFIG_HOME
  # that is unset now wrote its code — and any profiles that lived there
  # before a later state redirect — under the recorded root, and a report
  # derived only from the environment would count zero profiles beside the
  # real ones, skip the plugin repair there, and hand the user an rm for a
  # directory holding nothing while the real install root was named nowhere.
  # The environment's own root wins whenever it holds the runtime: moving the
  # config dir and re-exporting the variable is an environment fact, and a
  # record still pointing at the old root must not resurrect it. A record that
  # is unreadable, relative, or names no readable ccmorph.sh is not an install
  # and changes nothing.
  if [[ ! -r "$CONFIG_DIR/lib/ccmorph.sh" && -r "$BIN_DIR/.ccmorph-libdir" ]]; then
    _un_rec=""
    { read -r _un_rec < "$BIN_DIR/.ccmorph-libdir" } 2>/dev/null || :
    [[ "$_un_rec" == /* && -r "$_un_rec/ccmorph.sh" ]] && CONFIG_DIR="${_un_rec:h}"
  fi
  # Whether the state root was redirected is a question about DIRECTORIES, not
  # strings — the same rule the copy step's CONFIG_REAL comparison applies
  # below. A CCMORPH_STATE_DIR that spells the config dir with a trailing slash
  # or through a symlink names one directory; a string compare would count it
  # as a redirect and print the same profiles, keychain refs and rm targets
  # twice (#187).
  _un_redirected=0
  [[ "${_un_root:A}" != "${CONFIG_DIR:A}" ]] && _un_redirected=1
  # Claude Code records a plugin installed from a profile session under that
  # profile's spelling of the shared ~/.claude/plugins store. Every ccmorph run
  # repairs that, but there is no run after this one: repair now, so the plugin
  # still loads in native claude once the state root is gone. Best effort — a
  # missing or older core (no such subcommand) must not stop the uninstall.
  # Every profile home the report below can name for deletion gets the repair,
  # as its own two-operand call: the state root's and the code dir's, each as
  # spelled and as resolved, since the rm line names the resolved path when a
  # root is a link (#187, #207) — a redirect moves nothing, so the profiles
  # from before it still live there, and an index entry they spelled dangles
  # once the printed cleanup runs. Never one call with more operands:
  # the core run here is the INSTALLED one, which an older release may still
  # supply, and a verb whose operand count changed answers exit 2 and repairs
  # nothing.
  if [[ -x "$CONFIG_DIR/libexec/ccmorph-core" ]]; then
    typeset -U _un_homes
    _un_homes=("$_un_root/profiles" "$CONFIG_DIR/profiles" "${_un_root:A}/profiles" "${CONFIG_DIR:A}/profiles")
    for _un_home in "${_un_homes[@]}"; do
      "$CONFIG_DIR/libexec/ccmorph-core" util normalize-plugin-index "$HOME" "$_un_home" >/dev/null 2>&1 || :
    done
  fi
  # The runtime-location record is the command's own wiring, not user data:
  # it leaves with the command it belongs to (#834).
  rm -f "$BIN_DIR/ccmorph" "$BIN_DIR/.ccmorph-libdir"
  print -r -- "Removed $BIN_DIR/ccmorph."
  _un_state="${XDG_STATE_HOME:-$HOME/.local/state}/ccmorph"
  _un_profiles=("$_un_root"/*.conf(N))
  # The confs a redirect that came later left in CONFIG_DIR: still user data,
  # read for their count, their keychain refs and their line on the report
  # exactly like the redirected root's (#165).
  _un_prior=()
  (( _un_redirected )) && _un_prior=("$CONFIG_DIR"/*.conf(N))
  # The spellings the report names and hands to rm. rm on a symlink removes only
  # the link, so a path whose last component is one must never be what the
  # printed cleanup carries: the profiles, credentials and session history would
  # survive it, removed as a link and named nowhere (#187, #207). Only the last
  # component matters: rm reaches through a symlinked parent, and the user's own
  # spelling stays readable. A link is named beside the cleanup, because it
  # outlives what it points at. Only ENOENT, ENOTDIR and ELOOP prove a link
  # dangles and has only itself to remove — the #204 rule, `.github/REVIEW.md`.
  # Any other failed stat means the target could not be asked (EACCES, a stalled
  # volume): nothing in it could be read or counted, the rm names where the link
  # leads unread, and the link stays, or following the cleanup detaches what the
  # mount still holds and names it nowhere (#219).
  typeset -U _un_links
  _un_links=()
  typeset -U _un_unread
  _un_unread=()
  # `$ERRNO`/`$errnos` name the stat failure; without the module they stay empty,
  # which counts as "could not be asked", never as proof of a dangling link.
  zmodload zsh/system 2>/dev/null || :
  _un_unlinked() {
    local p="${1:a}" t n first="" e=""
    REPLY="$1"
    [[ -L "$p" ]] || return 0
    ERRNO=0
    if [[ -e "$p" ]]; then
      REPLY="${p:A}"
      _un_links+=("$p is a symlink to $REPLY and stays: it is yours to remove.")
      return 0
    fi
    # Read before another command can overwrite it.
    if (( ERRNO )); then e="${errnos[ERRNO]-}"; fi
    # Follow the link a hop at a time: `${p:A}` cannot resolve a target it
    # cannot read and would answer with the link itself, while readlink names
    # each hop without touching the target. 40 hops is the kernel's own limit.
    t="$p"
    repeat 40; do
      [[ -L "$t" ]] || break
      n="$(command readlink "$t" 2>/dev/null || :)"
      [[ -n "$n" ]] || break
      [[ "$n" == /* ]] || n="${t:h}/$n"
      [[ -n "$first" ]] || first="$n"
      t="$n"
    done
    if [[ "$e" == (ENOENT|ENOTDIR|ELOOP) ]]; then
      REPLY="$p"
      _un_links+=("$p is a symlink to ${first:-an unreadable target}, which does not exist now: nothing there is counted above, and the rm removes only the link.")
    else
      REPLY="${t:a}"
      _un_unread+=("$REPLY")
      _un_links+=("$p is a symlink to ${first:-an unreadable target}, which could not be reached (${e:-unknown error}): nothing in it could be read or counted above, and the rm names it unread; the link stays: it is yours to remove.")
    fi
  }
  _un_unlinked "$_un_root"; _un_named="$REPLY"
  _un_unlinked "$CONFIG_DIR"; _un_code_dir="$REPLY"
  # The config dir's own spelling whenever the two are one directory. The repair
  # above still ran under every other spelling too: a session's index entries
  # sit under whichever spelling it ran with.
  (( _un_redirected )) || _un_named="$_un_code_dir"
  _un_rm=("$_un_named")
  # With the state root redirected, the code the installer placed in CONFIG_DIR
  # is residue too: named as code — never as the profiles' home — and on the
  # same rm line, or ccmorph's own files would be left for the user to discover
  # later.
  [[ -d "$CONFIG_DIR" ]] && (( _un_redirected )) && _un_rm+=("$_un_code_dir")
  _un_state_named="$_un_state"
  # The usage-receipts root gets the state root's treatment whether it answers or
  # not: a dangling link has receipts nowhere, but it is on the report and the rm
  # line all the same, or the link is named nowhere and removed by nothing (#219).
  _un_has_state=0
  [[ -d "$_un_state" || -L "$_un_state" ]] && _un_has_state=1
  if (( _un_has_state )); then
    _un_unlinked "$_un_state"; _un_state_named="$REPLY"
    _un_rm+=("$_un_state_named")
  fi
  # The keychain items are read off the confs, never by scanning the keychain
  # (which prompts): a conf is the only thing that names them, so every conf
  # counted above is read — a redirect that came later must not hide the items
  # its pre-override confs point at (#165). `|| :` because a grep with no match
  # exits 1, an assignment takes its substitution's status, and `set -e` would
  # end the uninstall right there, mid-report.
  _un_keychain=()
  if [[ "$(uname -s 2>/dev/null)" == Darwin ]]; then
    _un_keychain=(${(f)"$(command grep -hsE '^(api_key|token)=keychain:.' "${_un_profiles[@]}" "${_un_prior[@]}" /dev/null || :)"})
    _un_keychain=(${_un_keychain#*=keychain:})
    _un_keychain=(${_un_keychain:#})
  fi
  # The profile count is a fact only when the root could be read. A link whose
  # target could not be asked has an unknown count: printing 0 as fact is how the
  # profiles were detached and named nowhere before (#219).
  _un_root_label="${#_un_profiles} profiles, their credentials and session history"
  (( ${_un_unread[(Ie)$_un_named]:-0} )) && _un_root_label="not read, so its profiles, credentials and session history are not counted"
  print -r -- "Kept, because they are yours:"
  print -r -- "  $_un_named  ($_un_root_label)"
  # The code dir's label: code alone — unless the redirect came later and left
  # profiles in it, in which case they are named beside the code, or following
  # the rm below deletes a profile the report never counted (#165).
  _un_code="the installed code"
  (( ${#_un_prior} )) && _un_code="the installed code, and ${#_un_prior} profile(s), their credentials and session history, from before the state root was redirected"
  [[ -d "$CONFIG_DIR" ]] && (( _un_redirected )) && print -r -- "  $_un_code_dir  ($_un_code)"
  (( _un_has_state )) && print -r -- "  $_un_state_named  (usage receipts)"
  (( ${#_un_keychain} )) && print -r -- "  login keychain  (${#_un_keychain} ccmorph item(s))"
  print -r -- "To remove those too:"
  print -r -- "  rm -rf" "${(q-)_un_rm[@]}"
  for _un_ref in "${_un_keychain[@]}"; do
    print -r -- "  security delete-generic-password -s ccmorph -a ${(q-)_un_ref}"
  done
  (( ${#_un_links} )) && print -rl -- "${_un_links[@]}"
  print -r -- "Plugins installed from a profile live in $HOME/.claude/plugins and keep working in claude."
  print -r -- "Open a new shell to finish."
  exit 0
fi


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
  # vendor. Claude Code gets the one sentence — with the setup link — that run
  # and doctor print for the same missing program (#282), and no command at all.
  _pkg_cmd zsh;     (( ${missing[(I)zsh]} ))     && (( ${#reply} )) && print "  zsh:     ${reply[*]}"
  (( ${missing[(I)claude*]} )) && print "Claude Code is not installed or not on the PATH — install it: https://docs.anthropic.com/en/docs/claude-code/setup"
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
  local answer
  print -n "Install now? $* [y/N] "
  read -r answer
  [[ "${answer:l}" == (y|yes) ]] || { c_dim "  skipped. Run yourself when ready: $*"; return 1 }
  "$@" || { c_r "  '$*' failed — install it by hand, then re-run this installer."; return 1 }
  return 0
}

# The dialect translator: a gateway's non-Anthropic models (gpt-*, kimi, glm,
# qwen) speak OpenAI dialects Claude Code cannot, and the adapter shells out to
# this to translate; Claude-family models need none of it, which is why this is
# a note and not a hard requirement. The note itself stays plain and names no
# profile setting: a first install has no profile yet, so `adapter=true` is a
# word the reader has never seen — it says what the tool is FOR, and that
# nothing else needs it (#273).
# homebrew/core, upstream github.com/router-for-me/CLIProxyAPI, no runtime deps.
if ! command -v cliproxyapi >/dev/null 2>&1; then
  c_y "note: cliproxyapi not found."
  c_dim "  It is optional — needed only to use non-Anthropic models (gpt, kimi,"
  c_dim "  glm, qwen) from a gateway with Claude Code. Nothing else needs it."
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

# ccmorph-core is compiled Go, so it is the component that is not a tracked
# file. A release tarball carries one prebuilt binary per
# platform (<name>-<os>-<arch>, from `make dist`); a checkout carries the ones
# `make shim` built. Nothing is compiled here: an installer that needs a Go
# toolchain on a user's machine is a second dependency for one helper.
#
# Resolved BEFORE the first copy. A tree without it (a GitHub tag archive
# carries only libexec/.gitkeep) is refused with the install it found still
# whole — lib and core the old pair — not after the copy loop swapped lib/ and
# then died on the missing binary (#721). The remote block above makes the same
# check on a download before handing it to its own installer, which may be an
# older release's.
_shim_os="${$(uname -s):l}"
_shim_arch="$(uname -m)"
case "$_shim_arch" in x86_64) _shim_arch=amd64 ;; aarch64) _shim_arch=arm64 ;; esac
_core_src=""   # the resolved ccmorph-core the copy below came from
for _name in ccmorph-core; do
  _shim=""
  for _f in "$SRC/libexec/$_name-$_shim_os-$_shim_arch" "$SRC/libexec/$_name"; do
    [[ -f "$_f" && -x "$_f" ]] && { _shim="$_f"; break }
  done
  [[ "$_name" == ccmorph-core ]] && _core_src="$_shim"
  if [[ -z "$_shim" ]]; then
    c_r "No $_name binary for $_shim_os-$_shim_arch beside this installer."
    c_dim "  From a checkout: run 'make shim' (needs Go), then re-run ./install.sh."
    c_dim "  Otherwise install a release: it ships the binary for your platform."
    c_dim "  Nothing was changed."
    exit 1
  fi
done

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
  mkdir -p "$CONFIG_DIR"/{bin,lib,libexec,share/claude-plugin/{.claude-plugin,commands,hooks},share/claude-plugin-router/{.claude-plugin,hooks,skills/router},share/zsh/site-functions}
  for _f in "${COMPONENTS[@]}"; do cp "$SRC/$_f" "$CONFIG_DIR/$_f"; done
fi
# A rename, not a copy over it: `ccmorph update` runs this from the launcher
# it replaces, and zsh reads a running script as it goes, so bytes rewritten
# under it ended the update with a parse error at exit 1. The rename leaves
# the running shell its old file.
cp "$SRC/bin/ccmorph" "$BIN_DIR/.ccmorph.new.$$" && mv -f "$BIN_DIR/.ccmorph.new.$$" "$BIN_DIR/ccmorph"
# The opt-in reference, so a host can see every seam without reading the source.
# Copied, never activated: local.sh itself stays absent until someone writes it.
# Same directory guard as above: in-place, this is a copy onto itself.
if [[ "$SRC" != "$CONFIG_REAL" ]]; then
  cp "$SRC/share/local.sh.example" "$CONFIG_DIR/local.sh.example"
  cp "$SRC/share/profile.conf.example" "$CONFIG_DIR/profile.conf.example"
fi
chmod +x "${EXECUTABLES[@]/#/$CONFIG_DIR/}"
# Place the core resolved above, once its directory exists.
for _name in ccmorph-core; do
  _shim="$_core_src"
  # Remove first: copying over a running-signed Mach-O in place keeps the kernel's stale
  # code-signature cache for the old bytes, and macOS then kills the new binary (exit 137).
  [[ "${_shim:A}" == "${CONFIG_REAL}/libexec/$_name" ]] \
    || { rm -f "$CONFIG_DIR/libexec/$_name" && cp "$_shim" "$CONFIG_DIR/libexec/$_name"; }
  chmod +x "$CONFIG_DIR/libexec/$_name"
done
chmod +x "$BIN_DIR/ccmorph"
# Record where the runtime lives, beside the command that must find it. The
# launcher searches this root after the one the CURRENT environment names, so
# exporting XDG_CONFIG_HOME later — or unexporting it after an install made
# under it — cannot hide the runtime from `ccmorph` itself (#834). A hidden
# dotfile: the command directory holds commands, and this is the command's own
# wiring, not a command; the uninstall below removes it with the command.
print -r -- "$CONFIG_DIR/lib" > "$BIN_DIR/.ccmorph-libdir"
# Move profile homes through the same guarded owner used by normal launches.
# The SOURCE tree's lib, for the reason the migrate below states: same bytes,
# but macOS validates a freshly copied binary's signature on its first exec,
# and the install defers that to the first launch.
# A release tarball names its binary per platform, so the source lib finds no
# ccmorph-core there; only then does this step run the placed copy.
_secure_lib="$SRC/lib"
[[ -x "$SRC/libexec/ccmorph-core" ]] || _secure_lib="$CONFIG_DIR/lib"
CCMORPH_STATE_DIR="$CONFIG_DIR" CCMORPH_LIB_DIR="$_secure_lib" zsh -fc \
  'source "$CCMORPH_LIB_DIR/ccmorph.sh"; ccmorph _secure' || exit 1
# Consolidate legacy context state only after the owning tool is installed.
# The migration runs the SOURCE binary, not the copy just placed — same bytes,
# but macOS validates a freshly copied binary's signature on its first exec
# (a sub-second stall per file), so the install defers that to the first launch.
CCMORPH_STATE_DIR="$CONFIG_DIR" "$_core_src" model-window --migrate
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
CCMORPH_CMD=ccmorph   # how the Check line spells the command: bare once it is on PATH
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

# ---- Tab completion ---------------------------------------------------------
# zsh finds a completion by scanning fpath for a file whose first line names a
# command — but only once something has started the completion system, and zsh
# runs no compinit by itself (#363): a Mac's /etc/zshrc never does, so an
# fpath line alone leaves Tab completing nothing. The file ships in
# $CONFIG_DIR/share/zsh/site-functions, a directory no stock zsh searches — so,
# like PATH, this is checked and reported, never arranged: the hint prints
# exactly the line or lines the rc still misses, the fpath line above compinit
# (or Oh My Zsh, which runs it), the compinit line under the fpath line. bash
# and fish are told nothing, and a .zshrc that already has both halves is
# read, never written. Homebrew links the same file into its own
# site-functions, which `brew shellenv` puts on fpath; `ccmorph doctor` asks a
# new zsh directly whether Tab completes and prints the missing lines
# (cli/doctor.go).
_comp_dir="$CONFIG_DIR/share/zsh/site-functions"
_zshrc="${ZDOTDIR:-$HOME}/.zshrc"
_comp_wanted=1
_login_shell="${SHELL:-}"
case "${_login_shell:t}" in
  bash|fish) _comp_wanted=0 ;;
esac
_comp_dir_bound=0
_compinit_in_rc=0
# Doctor's one fpath rule, asked of the core that owns it (#638): the rc names
# the directory (or ccmorph's site-functions), or the live FPATH already holds
# it or a directory with a _ccmorph file. An absent rc reads as empty, as
# doctor reads it, so an FPATH exported from .zshenv still counts.
if "$_core_src" util fpath-bound "$_zshrc" "$_comp_dir"; then
  _comp_dir_bound=1
fi
if [[ -f "$_zshrc" ]]; then
  # What the rc runs, not what it mentions (#449): a # that begins a word
  # starts a comment, so a commented-out compinit starts nothing, and only the
  # Oh My Zsh loader — not the directory's name — runs compinit for the rc.
  # Asked of the core that owns the rule, doctor's parser (#555): a zsh
  # re-spelling of the scan here once disagreed with doctor on an rc whose
  # last line has no trailing newline, on extendedglob from the user's .zshenv
  # (this installer runs under zsh, so it applies here), and on a # inside
  # quotes. The source core — the bytes just placed, so macOS does not
  # revalidate a fresh copy's signature — answers by exit status.
  if "$_core_src" util compinit-runs "$_zshrc"; then
    _compinit_in_rc=1
  fi
fi
(( _comp_dir_bound && _compinit_in_rc )) && _comp_wanted=0
if (( _comp_wanted )); then
  print ""
  if (( _compinit_in_rc )); then
    c_y "Tab completion for ccmorph is in $_comp_dir, which zsh does not search yet."
    c_dim "  Add this line to $_zshrc yourself, above the line that runs compinit (or loads Oh My Zsh); Tab completes in the next zsh you start:"
    print -r -- "  fpath=(${(q)_comp_dir} \$fpath)"
  elif (( _comp_dir_bound )); then
    c_y "$_zshrc puts $_comp_dir on fpath, but nothing in it starts zsh's completion system."
    c_dim "  Add this line to $_zshrc yourself, under the fpath line; Tab completes in the next zsh you start:"
    print -r -- "  autoload -Uz compinit && compinit"
  else
    c_y "Tab completion for ccmorph is in $_comp_dir, which zsh does not search — and nothing starts zsh's completion system."
    c_dim "  Add these two lines to $_zshrc yourself; Tab completes in the next zsh you start:"
    print -r -- "  fpath=(${(q)_comp_dir} \$fpath)"
    print -r -- "  autoload -Uz compinit && compinit"
  fi
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
# Off PATH the Check line carries the absolute path (it works right here), while
# the Next step stays the bare command the person will type once the shell is
# fixed — and never stands alone: the line above it says to run it in the new
# terminal, so nobody types it where 'ccmorph' is still not found (#273).
print "Check:  $CCMORPH_CMD doctor"
if (( ! IS_UPDATE )) && [[ "$CCMORPH_CMD" != ccmorph ]]; then
  c_dim "Run the next step in the new terminal above — this one cannot find 'ccmorph' yet:"
fi
(( IS_UPDATE )) || print "Next:   ccmorph add <name>"

# Context discovery is metadata-only and automatic on add/unknown launch.
# Installing must never start an inference sweep.
