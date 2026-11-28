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
# each profile's session history), the shared session history under ~/.claude,
# the usage receipts, and on macOS the keychain items the confs point at — so it
# is NAMED, with the command that removes it, rather than deleted here or left
# for them to discover later (#810).
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
  # dangles and has only itself to remove — the #204 rule, `REVIEW.md`.
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
  # The native claude layer, resolved: the one directory this report must never
  # hand to rm. A root, code dir or receipts link that resolves to it, contains
  # it (the home, /) or sits inside it would delete native settings and the
  # plugin store the report promises keep working (#810 gate). Two assignments
  # each: inside ${...} zsh reads `/.claude:A` as a ${name/pattern/repl}
  # substitution and yields the home.
  _un_home="${HOME:A}"
  _un_claude="$_un_home/.claude"
  _un_claude="${_un_claude:A}"
  # $1 = a resolved or lexically normalized link target. True when naming it for
  # deletion would reach the native layer. Both the path as given and its
  # resolution are compared: the unreadable branch can only normalize
  # lexically, and `~/.claude` is compared resolved (`/tmp` and `/private/tmp`
  # are one directory).
  _un_in_layer() {
    local r
    for r in "$1" "${1:A}"; do
      [[ "$r" == / || "$r" == "$_un_claude" ||
         "$_un_claude" == "$r"/* || "$r" == "$_un_claude"/* ]] && return 0
    done
    return 1
  }
  # The entry a spelling names, read the way the kernel reads it: the head
  # resolved PHYSICALLY, so a `..` after a symlink goes where the kernel goes,
  # and the last component kept unfollowed, so a link is still a link. A
  # trailing `/`, `//` or `/.` names that same entry. `${1:a}` cannot be used:
  # it collapses `..` lexically, so `alias/../state` (with `alias` a symlink)
  # names an unrelated entry instead of the root the override selects — an
  # override one reader honours and another forgets (`REVIEW.md`).
  # $1 = a spelling. REPLY = the entry it names; _un_spell = the same entry
  # without a trailing `/`, `//` or `/.` — what the report prints for it, since
  # `rm -rf path/.` is refused outright by BSD rm.
  _un_leaf() {
    local s="$1" head part hasdd=0
    # What the report prints when the entry is not a link: the caller's own
    # spelling, with any trailing `/.` removed (a trailing `/` alone is fine).
    _un_spell="$1"
    while [[ "$_un_spell" == */. ]]; do _un_spell="${_un_spell%/.}"; done
    while :; do
      s="${s%%/}"
      if [[ "$s" == */. ]]; then s="${s%/.}"; else break; fi
    done
    if [[ -z "$s" || "$s" == / ]]; then REPLY=/; return 0; fi
    if [[ "${s:t}" == .. ]]; then REPLY="${s:P}"; return 0; fi
    # A `..` in the head is resolved PHYSICALLY, or the entry named is not the
    # one the kernel reaches (`alias/../state` with `alias` a link). Without
    # one the caller's own spelling names the same entry and reads better: a
    # `/tmp/...` path stays `/tmp/...` rather than `/private/tmp/...`.
    head="${s:h}"
    for part in ${(s:/:)head}; do
      [[ "$part" == .. ]] && { hasdd=1; break; }
    done
    if (( hasdd )); then
      [[ "$head" == / ]] || head="${head:P}"
    else
      head="${head:a}"
    fi
    if [[ "$head" == / ]]; then REPLY="/${s:t}"; else REPLY="$head/${s:t}"; fi
    return 0
  }
  _un_unlinked() {
    local p t n first="" e=""
    _un_leaf "$1"; p="$REPLY"; REPLY="$_un_spell"
    [[ -L "$p" ]] || return 0
    ERRNO=0
    if [[ -e "$p" ]]; then
      t="${p:A}"
      if _un_in_layer "$t"; then
        # The link under its normalized spelling, not the resolved one: the rm
        # removes only the link, and the target is named beside it (#810 gate).
        # $p, never $1: a CCMORPH_STATE_DIR spelled `link/`, `link//` or `link/.`
        # hands rm a path it follows into the target, deleting ~/.claude's files.
        REPLY="$p"
        _un_links+=("$p is a symlink to $t, which sits inside ~/.claude or contains it: naming it would remove native claude's own files, so the rm removes only the link; $t stays: it is yours to resolve.")
        return 0
      fi
      REPLY="$t"
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
      if _un_in_layer "$REPLY"; then
        # An unreadable target that would reach the native layer is never named:
        # the rm carries only the link, and the count stays unknown (#810 gate).
        REPLY="$p"
        _un_unread+=("$p")
        _un_links+=("$p is a symlink to ${first:-an unreadable target}, which could not be reached (${e:-unknown error}) and may be or contain ~/.claude: nothing in it is named, and the rm removes only the link; it is yours to resolve.")
        return 0
      fi
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
  # The shared session history (#177): transcripts, prompt history and pasted
  # text — the three stores every profile links into ~/.claude, which native
  # claude reads and writes too. It outlives the rm above, so it is named with
  # it (#810); only what exists is named. A store that is a real file or
  # directory is named as spelled: rm reaches through a symlinked ~/.claude to
  # the real store. A store that is itself a symlink is named by where it leads
  # only when _un_hist_ok proves that target is the store moved elsewhere;
  # every other target, and a link that cannot be followed, never reaches the
  # rm: the rm removes only the link, and the target is named beside it.
  # _un_home and _un_claude were resolved above, before the root resolver.
  _un_plugins="$HOME/.claude/plugins"; _un_plugins="${_un_plugins:A}"
  # $1 = a fully resolved link target, $2 = the store's name. True only when
  # the target carries the store's own name and kind (history.jsonl a file,
  # projects and paste-cache directories), lies outside ~/.claude, and is
  # neither /, the home, ~/.claude nor the plugin store, nor a directory above
  # any of them — so following the printed rm never deletes native claude's
  # files, the plugins this report promises keep working, or the home.
  _un_hist_ok() {
    local t="$1" x
    [[ "$t" != / && "${t:t}" == "$2" ]] || return 1
    if [[ "$2" == history.jsonl ]]; then
      [[ -f "$t" ]] || return 1
    else
      [[ -d "$t" ]] || return 1
    fi
    [[ "$t" == "$_un_claude"/* ]] && return 1
    for x in "$_un_home" "$_un_claude" "$_un_plugins"; do
      [[ "$t" == "$x" || "$x" == "$t"/* ]] && return 1
    done
    return 0
  }
  # $1 = the store's name under ~/.claude. REPLY = the path the kept line and
  # the rm name for it.
  _un_hist_named() {
    local s="$HOME/.claude/$1" t
    REPLY="$s"
    [[ -L "$s" ]] || return 0
    if [[ -e "$s" ]]; then
      t="${s:A}"
      if _un_hist_ok "$t" "$1"; then
        REPLY="$t"
        _un_links+=("$s is a symlink to $t and stays: it is yours to remove.")
      else
        _un_links+=("$s is a symlink to $t, which this report cannot vouch for as session history: the rm removes only the link; $t stays: it is yours to check.")
      fi
      return 0
    fi
    t="$(command readlink "$s" 2>/dev/null || :)"
    _un_links+=("$s is a symlink to ${t:-an unreadable target}, which could not be followed (it does not exist now, or could not be read): nothing there is counted, and the rm removes only the link.")
    return 0
  }
  _un_hist=()
  for _un_h in history.jsonl projects paste-cache; do
    [[ -e "$HOME/.claude/$_un_h" || -L "$HOME/.claude/$_un_h" ]] || continue
    _un_hist_named "$_un_h"
    _un_hist+=("$REPLY")
  done
  (( ${#_un_hist} )) && _un_rm+=("${_un_hist[@]}")
  # The last word on the printed rm (#810 gate). Whatever produced an operand,
  # following the printed `rm -rf` must never delete or unlink what native
  # claude reads through ~/.claude. Two sets hold that, both built before any
  # operand is judged:
  #  - _un_trees: ~/.claude and the target of every symlink in it, link after
  #    link (a link in a target is followed too). An operand inside one deletes
  #    native claude's files — except the three history stores directly under
  #    ~/.claude, the rm's own operands, which are never searched for links (a
  #    link that leads into one makes what it leads to a tree of its own). A
  #    tree that is the home or holds it cannot be searched: when ~/.claude is
  #    one, or a link in a tree leads to one, nothing bounds what native claude
  #    reads, and no operand reaches the rm at all (_un_unbounded).
  #  - _un_dep: every directory entry the kernel visits resolving the home,
  #    ~/.claude, ~/.claude/plugins, ~/.claude.json and each of those symlinks
  #    — every component of the path and of every link target on the way, not
  #    only the last (#810 gate: a mid-path link is still a link claude is
  #    reached through). An operand that is one of them, or holds one, cuts
  #    native claude off from it.
  # Judged by file identity, never by spelling alone: a case-folded,
  # firmlinked or bind-mounted spelling of ~/.claude is still ~/.claude. An
  # operand that fails is left off the rm and named on its own line; its kept
  # line stays, and a line that said the rm removes it is withdrawn.
  # Two assignments each, never ${${x}/y:P}: see _un_claude above.
  _un_p_home="${HOME:P}"
  _un_p_claude="$HOME/.claude"; _un_p_claude="${_un_p_claude:P}"
  # A link's target is read with zsh's own zstat where the module loads, so a
  # home with many links costs no process per link; readlink(1) otherwise.
  _un_zstat=0
  zmodload -F zsh/stat b:zstat 2>/dev/null && _un_zstat=1
  # $1 = a symlink. REPLY = its target as spelled; empty when it cannot be read.
  _un_readlink() {
    local -a l
    REPLY=""
    if (( _un_zstat )); then
      if zstat -A l -L +link -- "$1" 2>/dev/null; then REPLY="${l[1]-}"; fi
    else
      REPLY="$(command readlink "$1" 2>/dev/null || :)"
    fi
    return 0
  }
  typeset -U _un_trees
  _un_trees=("$_un_p_claude")
  # $1 = an entry. True when it lies inside a tree, outside the history stores:
  # an operand above it is inside that tree, or is it, and is refused for that
  # already, so the entry needs no record. A history-store entry is recorded:
  # the rm may name a store, never one native claude is reached through.
  _un_quiet() {
    local t rel
    for t in "${_un_trees[@]}"; do
      [[ "$1" == "$t"/* ]] || continue
      if [[ "$t" == "$_un_p_claude" ]]; then
        rel="${1#"$t"/}"; rel="${(L)rel%%/*}"
        if [[ "$rel" == (history.jsonl|projects|paste-cache) ]]; then continue; fi
      fi
      return 0
    done
    return 1
  }
  # _un_dep is a set (its keys), so a home with many links stays linear.
  typeset -A _un_dep
  _un_dep=()
  # $1 = a physical directory, $2 = a path read from it (an absolute one from
  # /). Records in _un_dep every entry the kernel visits resolving $2, each as
  # its physical parent plus its own name — the form _un_rm_reach gives a link
  # operand. A `..` steps to the physical parent, as the kernel's does; a
  # relative target is read from the link's own directory. At most 40 links
  # (the kernel's own limit); a link that cannot be read ends the walk.
  _un_walk() {
    local cur="$1" c n hops=0
    local -a rest
    if [[ "$2" == /* ]]; then cur=/; fi
    rest=(${(s:/:)2})
    while (( ${#rest} )); do
      c="${rest[1]}"; shift rest
      if [[ "$c" == . ]]; then continue; fi
      if [[ "$c" == .. ]]; then cur="${cur:h}"; continue; fi
      if [[ "$cur" == / ]]; then n="/$c"; else n="$cur/$c"; fi
      if ! _un_quiet "$n"; then _un_dep[$n]=1; fi
      if [[ -L "$n" ]]; then
        (( ++hops <= 40 )) || return 0
        _un_readlink "$n"
        [[ -n "$REPLY" ]] || return 0
        if [[ "$REPLY" == /* ]]; then cur=/; fi
        rest=(${(s:/:)REPLY} "${rest[@]}")
      else
        cur="$n"
      fi
    done
    return 0
  }
  # $1 = a physical path. True when it is, or lies inside, a tree the search
  # below lists in full: by spelling first (cheap, and the common case), then
  # by identity. A history store directly under ~/.claude, and anything inside
  # one, does not count as inside ~/.claude: the search never lists them, so a
  # link that leads there is listed as a tree of its own (#810 gate).
  _un_in_tree() {
    local a="$1" b="" t rel
    for t in "${_un_trees[@]}"; do
      if [[ "$a" == "$t" ]]; then return 0; fi
      [[ "$a" == "$t"/* ]] || continue
      if [[ "$t" == "$_un_p_claude" ]]; then
        rel="${a#"$t"/}"; rel="${(L)rel%%/*}"
        if [[ "$rel" == (history.jsonl|projects|paste-cache) ]]; then continue; fi
      fi
      return 0
    done
    while :; do
      for t in "${_un_trees[@]}"; do
        [[ "$a" -ef "$t" ]] || continue
        if [[ "$t" == "$_un_p_claude" && "${(L)b:t}" == (history.jsonl|projects|paste-cache) ]]; then continue; fi
        return 0
      done
      if [[ "$a" == / ]]; then return 1; fi
      b="$a"; a="${a:h}"
    done
  }
  # $1 = a physical path. True when it is the home or a directory above it.
  _un_holds_home() {
    local a="$_un_p_home"
    while :; do
      if [[ "$a" -ef "$1" ]]; then return 0; fi
      if [[ "$a" == / ]]; then return 1; fi
      a="${a:h}"
    done
  }
  for _un_x in "$HOME" "$HOME/.claude" "$HOME/.claude/plugins" "$HOME/.claude.json"; do
    _un_walk "${PWD:P}" "$_un_x"
  done
  # Every tree is searched once for symlinks, and each one's target joins the
  # trees. A tree that is the home or holds it is never searched — that would
  # list every file in the home, or on the disk — so nothing bounds what native
  # claude reads through it: when ~/.claude is one, or a link found in a tree
  # leads to one (the plugin store linked to `..` included), the search stops
  # and no operand reaches the rm below (#810 gate). _un_unbounded is that
  # directory, _un_unbounded_via the path native claude reaches it by.
  _un_unbounded=""; _un_unbounded_via=""
  if _un_holds_home "$_un_p_claude"; then
    _un_unbounded="$_un_p_claude"; _un_unbounded_via="$HOME/.claude"
  fi
  _un_i=1
  while [[ -z "$_un_unbounded" ]] && (( _un_i <= ${#_un_trees} )); do
    _un_t="${_un_trees[_un_i]}"; (( ++_un_i ))
    _un_ls=()
    for _un_e in "$_un_t"/*(DN); do
      # The history stores are the rm's own operands: a store that is a link,
      # and the links inside one, are judged by _un_hist_named, not here.
      if [[ "$_un_t" == "$_un_p_claude" && "${_un_e:t}" == (history.jsonl|projects|paste-cache) ]]; then
        continue
      fi
      if [[ -L "$_un_e" ]]; then
        _un_ls+=("$_un_e")
      elif [[ -d "$_un_e" ]]; then
        _un_ls+=("$_un_e"/**/*(@DN))
      fi
    done
    for _un_l in "${_un_ls[@]}"; do
      # The glob never follows a link, so the link's directory is physical:
      # the walk starts there, at the link itself.
      _un_walk "${_un_l:h}" "${_un_l:t}"
      _un_lt="${_un_l:P}"
      [[ -e "$_un_lt" ]] || continue
      if _un_holds_home "$_un_lt"; then
        _un_unbounded="$_un_lt"; _un_unbounded_via="$_un_l"
        break
      fi
      if _un_in_tree "$_un_lt"; then continue; fi
      _un_trees+=("$_un_lt")
    done
  done
  # Every recorded entry and every directory above one, once.
  typeset -A _un_seen
  _un_seen=()
  for _un_x in "${(k)_un_dep[@]}"; do
    while [[ "$_un_x" != / && -z "${_un_seen[$_un_x]-}" ]]; do
      _un_seen[$_un_x]=1
      _un_x="${_un_x:h}"
    done
  done
  _un_anc=("${(k)_un_seen[@]}")
  # $1 = an rm operand. REPLY = the physical path `rm -rf` on it deletes: the
  # link entry alone for a symlink spelled without a trailing slash, else the
  # tree the spelling resolves to (:P, never :A, whose lexical `..` can name
  # another directory than the one rm reaches).
  _un_rm_reach() {
    local h
    if [[ "$1" != */ && -L "$1" ]]; then
      h="${1:h}"; h="${h:P}"
      if [[ "$h" == / ]]; then REPLY="/${1:t}"; else REPLY="$h/${1:t}"; fi
    else
      REPLY="${1:P}"
    fi
    return 0
  }
  # $1, $2 = entries as _un_walk records them. True when they are one entry:
  # the same spelling; the same name, in any case, in one directory (a
  # case-folding volume reads them as one); or, when neither is a link, one
  # file by identity (a firmlink or a bind mount).
  _un_same() {
    if [[ "$1" == "$2" ]]; then return 0; fi
    if [[ "${(L)1:t}" == "${(L)2:t}" && "${1:h}" -ef "${2:h}" ]]; then return 0; fi
    if [[ ! -L "$1" && ! -L "$2" && "$1" -ef "$2" ]]; then return 0; fi
    return 1
  }
  # $1 = a path from _un_rm_reach. True when `rm -rf` on it deletes or unlinks
  # an entry in _un_dep, a directory above one, or anything inside _un_trees
  # but the three history stores directly under ~/.claude. Only ever called
  # inside `if`.
  _un_reaches_native() {
    local r="$1" a e t
    if [[ "$r" == / ]]; then return 0; fi
    for a in "${_un_anc[@]}"; do
      if _un_same "$a" "$r"; then return 0; fi
    done
    a="$r"
    while [[ "$a" != / ]]; do
      e="${a:h}"
      for t in "${_un_trees[@]}"; do
        [[ "$e" -ef "$t" ]] || continue
        if [[ "$a" == "$r" && "$t" == "$_un_p_claude" && "${(L)a:t}" == (history.jsonl|projects|paste-cache) ]]; then
          continue
        fi
        return 0
      done
      a="$e"
    done
    return 1
  }
  _un_rm_ok=()
  for _un_o in "${_un_rm[@]}"; do
    if [[ -n "$_un_unbounded" ]]; then
      _un_links=(${_un_links:#"$_un_o is a symlink to "*})
      _un_links+=("$_un_o is left off the rm: $_un_unbounded_via leads to $_un_unbounded, the home or a directory above it, and what native claude reads through it cannot be searched, so nothing is put on the rm; what ccmorph wrote there stays: remove it by hand.")
      continue
    fi
    _un_rm_reach "$_un_o"
    if _un_reaches_native "$REPLY"; then
      # A line that said the rm removes this link no longer holds.
      _un_links=(${_un_links:#"$_un_o is a symlink to "*})
      _un_why="$REPLY"
      if [[ "$_un_o" != */ && -L "$_un_o" ]]; then _un_why="$REPLY (a symlink to ${_un_o:P})"; fi
      _un_links+=("$_un_o is left off the rm: rm -rf on it would delete $_un_why, which native claude reads: it is the home, ~/.claude or ~/.claude.json, holds one of them, lies inside ~/.claude or what a link in it leads to, or is a link or directory on the way to one of them; what ccmorph wrote there stays: remove it by hand.")
    else
      _un_rm_ok+=("$_un_o")
    fi
  done
  _un_rm=("${_un_rm_ok[@]}")
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
  # Unless CCMORPH_STATE_DIR moved it, the state root IS the install directory:
  # the runnable program copy sits beside the profiles. Its kept line says so,
  # or a person told "only your data stays" either deletes the profiles with
  # the code or keeps a runnable ccmorph they believe is gone (#810).
  if (( ! _un_redirected )) && [[ -f "$CONFIG_DIR/lib/ccmorph.sh" ]]; then
    _un_root_label="$_un_root_label, and ccmorph's own program files: bin, lib, libexec, share, install.sh"
  fi
  (( ${_un_unread[(Ie)$_un_named]:-0} )) && _un_root_label="not read, so its profiles, credentials and session history are not counted"
  print -r -- "Kept, because they are yours:"
  print -r -- "  $_un_named  ($_un_root_label)"
  # The code dir's label: code alone — unless the redirect came later and left
  # profiles in it, in which case they are named beside the code, or following
  # the rm below deletes a profile the report never counted (#165).
  _un_code="the installed code"
  (( ${#_un_prior} )) && _un_code="the installed code, and ${#_un_prior} profile(s), their credentials and session history, from before the state root was redirected"
  [[ -d "$CONFIG_DIR" ]] && (( _un_redirected )) && print -r -- "  $_un_code_dir  ($_un_code)"
  (( ${#_un_hist} )) && print -r -- "  ${(j:, :)_un_hist}  (shared session history, shared with claude)"
  (( _un_has_state )) && print -r -- "  $_un_state_named  (usage receipts)"
  (( ${#_un_keychain} )) && print -r -- "  login keychain  (${#_un_keychain} ccmorph item(s))"
  if (( ${#_un_rm} + ${#_un_keychain} )); then
    print -r -- "To remove those too:"
  fi
  if (( ${#_un_rm} )); then
    print -r -- "  rm -rf" "${(q-)_un_rm[@]}"
  fi
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

# _replace SRC DST: put SRC at DST by a rename, never by writing over DST.
# `ccmorph update` runs from the launcher and library it replaces, and zsh
# reads a running script as it goes, so bytes rewritten under it ended the
# update with a parse error at exit 1; the rename leaves the running shell its
# old file. A symlinked DST, dangling or not, keeps its link: the chain is
# followed and its last target replaced, staged beside it so the rename stays
# on one filesystem. An existing DST keeps its exact mode; a new one takes
# SRC's. A failed step leaves no temp file, DST untouched, and fails the
# install.
zmodload -F zsh/stat b:zstat
_replace() {
  local src="$1" dst="$2" tmp link hops=0
  local -a mode
  while [[ -L "$dst" ]] && (( hops++ < 40 )); do
    link="$(readlink -- "$dst")" || break
    [[ "$link" == /* ]] || link="${dst:h}/$link"
    dst="$link"
  done
  tmp="${dst:h}/.${dst:t}.new.$$"
  { cp "$src" "$tmp" \
    && { [[ ! -e "$dst" ]] || { zstat -A mode +mode -- "$dst" && chmod "$(( [##8] mode[1] & 8#7777 ))" "$tmp"; }; } \
    && mv -f "$tmp" "$dst"; } \
    || { rm -f "$tmp"; print -u2 -- "ccmorph install: could not place $dst"; return 1; }
}

# If running from inside the target dir, files are already home; otherwise copy.
if [[ "$SRC" != "$CONFIG_REAL" ]]; then
  mkdir -p "$CONFIG_DIR"/{bin,lib,libexec,share/claude-plugin/{.claude-plugin,commands,hooks},share/claude-plugin-router/{.claude-plugin,hooks,skills/router},share/zsh/site-functions}
  for _f in "${COMPONENTS[@]}"; do _replace "$SRC/$_f" "$CONFIG_DIR/$_f"; done
fi
_replace "$SRC/bin/ccmorph" "$BIN_DIR/ccmorph"
# The opt-in reference, so a host can see every seam without reading the source.
# Copied, never activated: local.sh itself stays absent until someone writes it.
# Same directory guard as above: in-place, this is a copy onto itself.
if [[ "$SRC" != "$CONFIG_REAL" ]]; then
  _replace "$SRC/share/local.sh.example" "$CONFIG_DIR/local.sh.example"
  _replace "$SRC/share/profile.conf.example" "$CONFIG_DIR/profile.conf.example"
fi
chmod +x "${EXECUTABLES[@]/#/$CONFIG_DIR/}"
# Place the core resolved above, once its directory exists.
for _name in ccmorph-core; do
  _shim="$_core_src"
  # A rename, never a copy in place: writing over a running-signed Mach-O keeps the
  # kernel's stale code-signature cache for the old bytes, and macOS then kills the
  # new binary (exit 137). The rename gives a new file and never leaves the path empty.
  [[ "${_shim:A}" == "${CONFIG_REAL}/libexec/$_name" ]] \
    || _replace "$_shim" "$CONFIG_DIR/libexec/$_name"
  chmod +x "$CONFIG_DIR/libexec/$_name"
done
chmod +x "$BIN_DIR/ccmorph"
# Record where the runtime lives, beside the command that must find it. The
# launcher searches this root after the one the CURRENT environment names, so
# exporting XDG_CONFIG_HOME later — or unexporting it after an install made
# under it — cannot hide the runtime from `ccmorph` itself (#834). A hidden
# dotfile: the command directory holds commands, and this is the command's own
# wiring, not a command; the uninstall below removes it with the command.
_replace =(print -r -- "$CONFIG_DIR/lib") "$BIN_DIR/.ccmorph-libdir"
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
_compinit_known=1
if [[ -f "$_zshrc" ]]; then
  # What the rc runs, not what it mentions (#449): a # that begins a word
  # starts a comment, so a commented-out compinit starts nothing, and only the
  # Oh My Zsh loader — not the directory's name — runs compinit for the rc.
  # Asked of the core that owns the rule, doctor's parser (#555): a zsh
  # re-spelling of the scan here once disagreed with doctor on an rc whose
  # last line has no trailing newline, on extendedglob from the user's .zshenv
  # (this installer runs under zsh, so it applies here), and on a # inside
  # quotes. The source core — the bytes just placed, so macOS does not
  # revalidate a fresh copy's signature — answers by exit status: 0 runs, 1
  # does not. Anything above 1 is not a verdict — the scan itself could not
  # run — and a failed scan is never read as "already started", which would
  # suppress the line that makes Tab work (#582).
  _compinit_status=0
  "$_core_src" util compinit-runs "$_zshrc" || _compinit_status=$?
  case $_compinit_status in
    0) _compinit_in_rc=1 ;;
    1) ;;
    *) _compinit_known=0 ;;
  esac
fi
(( _comp_dir_bound && _compinit_in_rc )) && _comp_wanted=0
if (( _comp_wanted )); then
  print ""
  if (( _compinit_in_rc )); then
    c_y "Tab completion for ccmorph is in $_comp_dir, which zsh does not search yet."
    c_dim "  Add this line to $_zshrc yourself, above the line that runs compinit (or loads Oh My Zsh); Tab completes in the next zsh you start:"
    print -r -- "  fpath=(${(q)_comp_dir} \$fpath)"
  elif (( _comp_dir_bound )); then
    if (( _compinit_known )); then
      c_y "$_zshrc puts $_comp_dir on fpath, but nothing in it starts zsh's completion system."
    else
      c_y "$_zshrc puts $_comp_dir on fpath, but whether it starts zsh's completion system could not be checked."
    fi
    c_dim "  Add this line to $_zshrc yourself, under the fpath line; Tab completes in the next zsh you start:"
    print -r -- "  autoload -Uz compinit && compinit"
  else
    if (( _compinit_known )); then
      c_y "Tab completion for ccmorph is in $_comp_dir, which zsh does not search — and nothing starts zsh's completion system."
    else
      c_y "Tab completion for ccmorph is in $_comp_dir, which zsh does not search — and whether zsh's completion system starts could not be checked."
    fi
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

# The installer copies the plugin that provides /ccmorph:hop, but only a session
# this launcher starts loads it: an installer that only said "Done" left a
# first-timer to find that out by absence in plain `claude` (#651).
c_dim "Sessions you start with ccmorph run automatically get /ccmorph:hop and a status line naming the profile; plain claude does not."

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
