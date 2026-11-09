#!/usr/bin/env zsh
# install.sh tests — hermetic: fake HOME + ZDOTDIR under a mktemp dir.
# The installer checks for `claude` on PATH; a stub satisfies it.

set -u
emulate -L zsh

REPO="${0:A:h:h}"
# Derived, never hand-listed: install.sh enumerates this same glob, so a module
# added beside ccmorph.sh is covered here with no edit — and a module dropped
# from the repo cannot leave a stale name behind in the manifest below.
module_files=("$REPO"/lib/ccmorph-*.zsh(N:t))   # not `modules`: a readonly zsh special

# ccmorph-core is compiled, so a checkout has none until
# `make shim` builds them; the installer copies those binaries and refuses to run
# without them. Before the shard fan-out below, so it runs once, not per shard.
[[ -x "$REPO/libexec/ccmorph-core" ]] || make -C "$REPO" shim >/dev/null 2>&1
[[ -x "$REPO/libexec/ccmorph-core" ]] || { print -r -- "FAIL: no libexec/ccmorph-core and 'make shim' did not build it (needs Go)"; exit 1 }

# --- shards -------------------------------------------------------------------
# Every case builds its own scratch home, so the suite runs as N shards at once
# (CCMORPH_INSTALL_JOBS=1 runs it serially). It is wait-bound — a hundred real
# installer runs, 62 s serial — so concurrent shards overlap the
# waits, the same shape the launch suite shards (#134). Sections that share a
# case dir (1b+2) form one unit; each unit is dealt longest-first onto the
# least-loaded shard from the measured seconds in _unit_cost, so a new section
# needs only its `if _own <name>` wrapper and a cost here. _own refuses a name
# missing from the table, and the parent fails unless every unit ran exactly
# once, so a typo costs a loud failure, not a silently lost section.
readonly _jobs_default=4
typeset -gA _unit_cost=(
  basics     3   # 1 + 1b + 2: the legacy check reuses 1b's case dir
  inplace    1   # 2b
  migrate    1   # 2c
  module     1   # 2d
  symlink    1   # 3
  uninstall  1   # 4
  residue    4   # 5
  statedir   3   # 5b + 5c: the CCMORPH_STATE_DIR redirect report
  linkspell  9   # 5d + 5e: equivalent spellings, links and the rm line
  rerun      1   # 6
  optdeps    2   # 7
  remote    12   # 8, incl. the make-dist release build
  windows    1   # 9
  hint       1   # 10
  pkgmgr     5   # 11
  path       7   # 12
  footer     4   # 13
  selfupdate 17  # 14
)
_own() {  # true when this process runs the unit; records it for the parent's check
  (( ${+_unit_cost[$1]} )) || { print -u2 -- "FAIL: unit '$1' is not in _unit_cost"; exit 1 }
  [[ -n "${CCMORPH_INSTALL_RUN_DIR:-}" ]] || return 0        # serial run: every unit
  (( ${_mine[(Ie)$1]} )) || return 1
  print -r -- "$1" >> "$CCMORPH_INSTALL_RUN_DIR/units.$CCMORPH_INSTALL_SHARD"
}
if [[ -z "${CCMORPH_INSTALL_SHARD:-}" ]] && (( ${CCMORPH_INSTALL_JOBS:-$_jobs_default} > 1 )); then
  typeset -gi _jobs=${CCMORPH_INSTALL_JOBS:-$_jobs_default}
  typeset -a _pids=(); typeset -A _load=() _deal=()
  typeset _u; typeset -gi _s _best _rc=0 _p=0 _f=0
  for _u in ${(kO)_unit_cost}; do
    _best=0
    for (( _s = 1; _s < _jobs; _s++ )); do
      (( ${_load[$_s]:-0} < ${_load[$_best]:-0} )) && _best=$_s
    done
    _load[$_best]=$(( ${_load[$_best]:-0} + _unit_cost[$_u] ))
    _deal[$_best]="${_deal[$_best]:-}$_u "
  done
  _run=$(mktemp -d /tmp/ccmorph-install-shard.XXXXXX)
  trap 'rm -rf "$_run"' EXIT
  for (( _s = 0; _s < _jobs; _s++ )); do
    CCMORPH_INSTALL_SHARD="$_s" CCMORPH_INSTALL_RUN_DIR="$_run" \
      CCMORPH_INSTALL_UNITS="${_deal[$_s]:-}" zsh "$0" &
    _pids+=($!)
  done
  for _pid in $_pids; do wait $_pid || _rc=1; done
  for (( _s = 0; _s < _jobs; _s++ )); do
    if [[ ! -s "$_run/count.$_s" ]]; then
      print -u2 -- "FAIL: shard $_s never finished"
      _rc=1; continue
    fi
    read -r _pp _ff < "$_run/count.$_s"; _p+=$_pp; _f+=$_ff
  done
  typeset -a _ran _want
  _ran=(${(o)$(cat "$_run"/units.*(N) 2>/dev/null)})
  _want=(${(k)_unit_cost}); _want=(${(o)_want})
  if [[ "${(j:,:)_ran}" != "${(j:,:)_want}" ]]; then
    print -u2 -- "FAIL: the shard deal changed the section set: ran [${_ran[*]:-none}], want [${_want[*]}]"
    _rc=1
  fi
  print ""
  print "install tests: $_p passed, $_f failed"
  exit $_rc
fi
typeset -ga _mine=(${=CCMORPH_INSTALL_UNITS:-})

TMP=$(mktemp -d /tmp/ccmorph-install-test.XXXXXX)
trap 'rm -rf "$TMP"' EXIT

passed=0; failed=0
ok()   { print -r -- "ok: $1"; (( ++passed )) }
fail() { print -r -- "FAIL: $1"; (( ++failed )) }

fresh_env() {  # $1 = case dir; prints nothing, sets HOME/ZDOTDIR/PATH stubs
  local d="$TMP/$1"
  mkdir -p "$d/home" "$d/bin"
  for t in zsh claude fzf; do
    printf '#!/bin/sh\nexit 0\n' > "$d/bin/$t"; chmod +x "$d/bin/$t"
  done
  # zsh stub would break the installer's own shebang usage — the installer is
  # invoked via `zsh $REPO/install.sh`, so PATH stubs only shadow checks.
  rm -f "$d/bin/zsh"
  print -r -- "$d"
}

run_installer() {  # $1 = case dir
  # </dev/null: the installer OFFERS missing optional deps
  # interactively. A test must neither block on that prompt nor let a suite
  # `brew install` anything on the machine running it.
  env -i HOME="$1/home" ZDOTDIR="$1/home" PATH="$1/bin:/usr/bin:/bin:/opt/homebrew/bin" \
    zsh "$REPO/install.sh" >"$1/out" 2>&1 </dev/null
}

# The download the remote-mode cases (§8, §9) and the self-update cases (§14)
# serve through a stub curl: shaped like the release asset the default URL
# serves — one top dir, `ccmorph-<tag>/`, exactly what `git archive --prefix`
# writes — so the remote block must accept it whatever the tag names. Built
# here, before the units, because those sections land on different shards.
mkdir -p "$TMP/stage/ccmorph-v1.2.3"
(cd "$REPO" && cp -R bin lib libexec share install.sh "$TMP/stage/ccmorph-v1.2.3/")
# The staged core binary is a stub: these cases assert the installer's
# orchestration (URL picked, tree unpacked, its installer run, state kept) and
# never run the downloaded code, while the 13 MB real one would put a
# signature-validation stall and a full untar into every one of the ~15 unpacks.
# Section 14 runs the downloaded code, so it stages the real binary itself.
printf '#!/bin/sh\nexit 0\n' > "$TMP/stage/ccmorph-v1.2.3/libexec/ccmorph-core"
tar -czf "$TMP/src.tar.gz" -C "$TMP/stage" ccmorph-v1.2.3
# Not a ccmorph tree: one top dir with no lib/ccmorph.sh in it.
mkdir -p "$TMP/stage-empty/not-ccmorph"; print x > "$TMP/stage-empty/not-ccmorph/README"
tar -czf "$TMP/empty.tar.gz" -C "$TMP/stage-empty" not-ccmorph

url_release='https://github.com/akleizor/ccmorph/releases/latest/download/ccmorph.tar.gz'

# One sentence, one link, wherever a missing claude is reported (#282): the
# installer prints it, and the same bytes sit in the core binary, which is what
# `ccmorph run` and `ccmorph doctor` print — checked here so the three cannot
# drift apart.
claude_missing='Claude Code is not installed or not on the PATH — install it: https://docs.anthropic.com/en/docs/claude-code/setup'

remote_env() {  # $1 = case name, $2 = tarball; stub curl bakes in its log and tarball
  local d; d=$(fresh_env "$1")
  mkdir -p "$d/tmp"
  printf '#!/bin/sh\nprintf "%%s\\n" "$@" >> "%s/curl.argv"\nwhile [ $# -gt 0 ]; do\n  if [ "$1" = -o ]; then cp "%s" "$2"; exit 0; fi\n  shift\ndone\nexit 1\n' "$d" "$2" > "$d/bin/curl"
  chmod +x "$d/bin/curl"
  print -r -- "$d"
}

remote_run() {  # $1 = case dir, $2 = cmd|pipe, rest = extra env assignments
  local d="$1" mode="$2"; shift 2
  rm -f "$d/curl.argv"
  if [[ "$mode" == pipe ]]; then
    cat "$REPO/install.sh" | env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" TMPDIR="$d/tmp" "$@" \
      sh >"$d/out" 2>"$d/err"
  else
    env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" TMPDIR="$d/tmp" "$@" \
      sh -c "$(cat "$REPO/install.sh")" >"$d/out" 2>"$d/err" </dev/null
  fi
}

# --- 1. fresh install lands every runtime component and wires the rc --------
if _own basics; then
d=$(fresh_env fresh)
run_installer "$d"; rc=$?
[[ $rc -eq 0 ]] && ok "fresh install exits 0" || { fail "fresh install rc=$rc"; cat "$d/out" }
# The installed surface is EXACTLY the manifest — nothing extra ever rides
# along (this is what keeps any other product from shipping inside ccmorph),
# and nothing missing either, which subsumes a per-component presence loop.
# The plugin subtree is pinned one level deeper than the rest of share/:
# its hooks file is what carries a session's end to the launcher (#285), so
# an installer that drops it must fail here, not in a user's terminal.
(( ${#module_files} )) || fail "no ccmorph-*.zsh module found beside ccmorph.sh — the manifest below would be silently short"
want=(bin/ccmorph lib/ccmorph.sh ${module_files/#/lib/} libexec/ccmorph-core share/model-catalogue.json share/window-sources.json share/provider-presets.tsv share/usage-export share/claude-plugin/.claude-plugin/plugin.json share/claude-plugin/commands/hop.md share/claude-plugin/commands/profile.md share/claude-plugin/hooks/hooks.json local.sh.example profile.conf.example install.sh profiles)
got=(${(f)"$(cd "$d/home/.config/ccmorph" && print -l *(N) {bin,lib,libexec,share}/*(N.) share/claude-plugin/*/*(ND.) | grep -vxE 'bin|lib|libexec|share')"})
extra=(${got:|want}); missing=(${want:|got})
(( ${#extra} == 0 && ${#missing} == 0 )) \
  && ok "install places exactly its own manifest" \
  || fail "manifest drift — extra: ${extra[*]:-none}, missing: ${missing[*]:-none}"
# The placed binary runs: it must start, not just exist.
"$d/home/.config/ccmorph/libexec/ccmorph-core" adapter --help >/dev/null 2>&1 \
  && ok "the installed ccmorph-core starts" || fail "the installed ccmorph-core does not start"
bin_got=("$d/home/.local/bin/"*(N:t))
[[ "${(j:,:)bin_got}" == "ccmorph" ]] \
  && ok "bin dir holds exactly the ccmorph command" || fail "bin dir: ${bin_got[*]}"
# The opt-in reference ships; the opt-in itself does not. An installed local.sh
# would silently turn features on that the installing host never asked for.
[[ -f "$d/home/.config/ccmorph/local.sh.example" ]] \
  && ok "opt-in reference installed" || fail "local.sh.example missing"
[[ -f "$d/home/.config/ccmorph/profile.conf.example" ]] \
  && ok "profile template installed" || fail "profile.conf.example missing"
[[ ! -e "$d/home/.config/ccmorph/local.sh" ]] \
  && ok "no local.sh activated on a fresh install" || fail "install activated local.sh"
# The rc must be left ALONE. ccmorph is one executable, so there is nothing to
# wire; an installer that edits a startup file makes every session's behaviour
# depend on whether one file was sourced.
grep -qF '# >>> ccmorph >>>' "$d/home/.zshrc" \
  && fail "installer wired the rc — ccmorph needs no shell integration" \
  || ok "rc left untouched"

# --- 1b. no ccmorph-core binary for this platform: install fails, says why -----
# The binary is the one component a checkout does not track. An installer that
# carried on without it would leave a launch that dies at its first request.
d=$(fresh_env nobinary)
mkdir -p "$d/src"
(cd "$REPO" && tar --exclude=./.git --exclude=./.claude --exclude=./dist --exclude='./libexec/ccmorph-core*' -cf - .) | tar -xf - -C "$d/src"
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin:/opt/homebrew/bin" \
  zsh "$d/src/install.sh" >"$d/out" 2>&1 </dev/null; rc=$?
(( rc != 0 )) && ok "install without a ccmorph-core binary exits non-zero" || fail "install without a binary exited 0"
grep -qF 'No ccmorph-core binary for' "$d/out" && grep -qF "make shim" "$d/out" \
  && ok "the missing binary is named, with the fix" || { fail "no clear message for a missing binary"; cat "$d/out" }
[[ ! -e "$d/home/.config/ccmorph/libexec/ccmorph-core" ]] \
  && ok "nothing half-installed as the shim" || fail "a shim was placed despite no binary"

# --- 2. legacy startup wiring is reported but never edited --------------------
print 'export BEFORE=1' > "$d/home/.zshrc"
print '# >>> ccmorph >>>' >> "$d/home/.zshrc"
print 'source "/old/path/lib/ccmorph.sh"' >> "$d/home/.zshrc"
print '# missing end marker' >> "$d/home/.zshrc"
print 'export AFTER=1' >> "$d/home/.zshrc"
original=$(<"$d/home/.zshrc")
run_installer "$d"; rc=$?
(( rc == 0 )) && ok "legacy startup block does not fail install" || fail "legacy block install rc=$rc"
[[ "$(<"$d/home/.zshrc")" == "$original" ]] && ok "installer leaves partial rc byte-identical" || fail "installer edited partial rc"
grep -qiF 'legacy ccmorph block' "$d/out" && ok "legacy rc ownership is reported" || fail "legacy rc was not reported"
fi

# --- 2b. an IN-PLACE install (clone already at the target) succeeds -----------
if _own inplace; then
# The normal update path on a machine whose repo lives at ~/.config/ccmorph:
# SRC == CONFIG_DIR, so every copy is a copy onto itself. cp refuses that, and
# under `set -e` the run aborted before the rc was ever wired — an update that
# reported failure and left the install half-applied. Exercised by installing
# from a COPY of the repo placed at the target path.
d=$(fresh_env inplace)
target="$d/home/.config/ccmorph"
mkdir -p "$target"
mkdir -p "$target"/{bin,lib,libexec,share}
for f in bin/ccmorph lib/ccmorph.sh ${module_files/#/lib/} libexec/ccmorph-core share/model-catalogue.json share/window-sources.json share/usage-export share/local.sh.example share/profile.conf.example install.sh; do
  cp "$REPO/$f" "$target/$f"
done
env -i HOME="$d/home" ZDOTDIR="$d/home" \
  PATH="$d/bin:/usr/bin:/bin:/opt/homebrew/bin" \
  zsh "$target/install.sh" >"$d/out2" 2>&1
rc=$?
[[ $rc -eq 0 ]] && ok "in-place install exits 0" \
  || { fail "in-place install rc=$rc"; cat "$d/out2" }
grep -qF '# >>> ccmorph >>>' "$d/home/.zshrc" \
  && fail "in-place install wired the rc" || ok "in-place install leaves the rc alone"
[[ -x "$d/home/.local/bin/ccmorph" ]] \
  && ok "in-place install still places the command" || fail "in-place install skipped the command"
fi

# --- 2c. upgrading migrates legacy registry data INTO the owning profile's conf --
if _own migrate; then
# An upgrade must never hand the user a chore. Installing over a pre-consolidation
# layout has to carry the probed/scoped registry data across on its own, before the
# first command runs — install.sh calls `ccmorph-core model-window --migrate`, which attributes
# each `<profile>:<model>=<value>` row to that profile's OWN conf (as
# context_discovered) and archives the legacy file with a .migrated suffix. There
# is no cross-profile fallback and no rename-in-place: a bare (unscoped) row with no
# owning profile is left unattributed rather than guessed at.
d=$(fresh_env upgrade)
mkdir -p "$d/home/.config/ccmorph"
print -r -- 'type=apikey
base_url=http://gw.invalid
api_key=sk_test' > "$d/home/.config/ccmorph/work.conf"
print -r -- 'work:claude-opus-5=1000000' > "$d/home/.config/ccmorph/model-registry.conf"
run_installer "$d"; rc=$?
(( rc == 0 )) || { fail "upgrade install rc=$rc"; cat "$d/out" }
[[ ! -e "$d/home/.config/ccmorph/model-registry.conf" ]] \
  && ok "migration archives the legacy registry rather than leaving it live" \
  || fail "the legacy registry survived under its live name, and will be parsed as a profile"
[[ -f "$d/home/.config/ccmorph/model-registry.conf.migrated" ]] \
  && ok "the legacy registry is kept as a .migrated backup" \
  || fail "migration destroyed the legacy registry instead of archiving it"
grep -qF '"models"' "$d/home/.config/ccmorph/profiles/work/models.json" \
  && ok "migration writes the probed window into the owning profile's models.json" \
  || fail "migration did not carry the scoped row into work's models.json"
grep -qF '1000000' "$d/home/.config/ccmorph/profiles/work/models.json" \
  && ok "migration preserves the probed value" \
  || fail "migration lost the probed window's value"

# Re-running install again (nothing left to migrate) must not error or re-migrate.
run_installer "$d"; rc=$?
(( rc == 0 )) || { fail "second install after migration rc=$rc"; cat "$d/out" }
[[ ! -e "$d/home/.config/ccmorph/model-registry" ]] \
  && ok "a second install does not resurrect the legacy registry" \
  || fail "a second install re-created the legacy registry"
fi

# --- 2d. a module added beside ccmorph.sh ships with no installer edit -------
if _own module; then
# The whole point of deriving the module set: parts 3–4 of the module split add
# more ccmorph-*.zsh files, and a name missed in the installer's manifest means
# a module that never ships — the library then fails to source at launch, in the
# operator's profile home, where no test can see it. The probe is inert: the
# loader names its modules explicitly, so nothing sources this one.
d=$(fresh_env probe)
mkdir -p "$d/src"
mkdir -p "$d/src"
(cd "$REPO" && cp -R bin lib libexec share *(.) "$d/src/")
print -r -- '# a module that exists only in this test' > "$d/src/lib/ccmorph-zz-probe.zsh"
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin:/opt/homebrew/bin" \
  zsh "$d/src/install.sh" >"$d/out" 2>&1 </dev/null
rc=$?
(( rc == 0 )) && ok "install from a tree with an extra module exits 0" \
  || { fail "probe install rc=$rc"; cat "$d/out"; }
[[ -f "$d/home/.config/ccmorph/lib/ccmorph-zz-probe.zsh" ]] \
  && ok "a module added beside ccmorph.sh ships with no installer edit" \
  || fail "an extra ccmorph-*.zsh was not installed — the module list is hand-copied again"
[[ ! -x "$d/home/.config/ccmorph/lib/ccmorph-zz-probe.zsh" ]] \
  && ok "an installed module is data, not an executable" \
  || fail "an installed module carries the exec bit"
fi

# --- 3. symlinked and partial startup files remain untouched ------------------
if _own symlink; then
d=$(fresh_env symlink)
mkdir -p "$d/dotfiles"
print 'export KEEP=1' > "$d/dotfiles/zshrc"
print '# >>> ccmorph >>>' >> "$d/dotfiles/zshrc"
ln -s "$d/dotfiles/zshrc" "$d/home/.zshrc"
original=$(<"$d/dotfiles/zshrc")
run_installer "$d"
[[ -L "$d/home/.zshrc" && "$(<"$d/dotfiles/zshrc")" == "$original" ]] && ok "symlinked rc stays byte-identical" || fail "installer edited symlinked rc"
fi

# --- 4. uninstall also leaves startup files untouched ------------------------
if _own uninstall; then
d=$(fresh_env uninstall)
run_installer "$d" >/dev/null
print '# >>> ccmorph >>>' >> "$d/home/.zshrc"
print 'export TAIL=1' >> "$d/home/.zshrc"
original=$(<"$d/home/.zshrc")
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" \
  zsh "$REPO/install.sh" --uninstall >/dev/null 2>&1
[[ "$(<"$d/home/.zshrc")" == "$original" ]] && ok "uninstall leaves rc byte-identical" || fail "uninstall edited rc"
[[ ! -e "$d/home/.local/bin/ccmorph" ]] && ok "uninstall removes the command" || fail "uninstall left the command on PATH"
fi

# --- 5. uninstall removes ONLY the command, repairs the plugin index, and names
# what it leaves with the command that removes it. The uname stub is placed
if _own residue; then
# after the install so only the uninstall sees "Darwin" (the keychain branch).
d=$(fresh_env uninstall_residue)
mkdir -p "$d/home/.local/bin"
printf '#!/bin/sh\necho mine\n' > "$d/home/.local/bin/ccrelay"   # a neighbour, not ours
run_installer "$d" >/dev/null
cfg="$d/home/.config/ccmorph"
[[ "$(<"$d/home/.local/bin/ccrelay")" == *mine* ]] && ok "install: a neighbour ccrelay in ~/.local/bin survives" || fail "install: deleted ~/.local/bin/ccrelay"
printf 'type=apikey\napi_key=keychain:work.api_key\nbase_url=https://example.invalid\n' > "$cfg/work.conf"
printf 'type=sub\n' > "$cfg/home.conf"
mkdir -p "$d/home/.local/state/ccmorph/usage" "$d/home/.claude/plugins/marketplaces/example"
# How Claude Code records a plugin installed from a profile session: the profile's
# spelling of the shared store, which dangles once the state root is deleted.
print -r -- '{"example": {"source": {"source": "github", "repo": "owner/repo"}, "installLocation": "'"$cfg"'/profiles/work/plugins/marketplaces/example"}}' \
  > "$d/home/.claude/plugins/known_marketplaces.json"
printf '#!/bin/sh\necho Darwin\n' > "$d/bin/uname"; chmod +x "$d/bin/uname"
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" \
  zsh "$REPO/install.sh" --uninstall >"$d/out" 2>&1; rc=$?
(( rc == 0 )) && ok "uninstall: exits 0 with residue to report" || { fail "uninstall rc=$rc"; cat "$d/out" }
[[ ! -e "$d/home/.local/bin/ccmorph" && -e "$d/home/.local/bin/ccrelay" ]] \
  && ok "uninstall: removes the command and nothing else in ~/.local/bin" || fail "uninstall: touched more than ccmorph in ~/.local/bin"
grep -qF "\"$d/home/.claude/plugins/marketplaces/example\"" "$d/home/.claude/plugins/known_marketplaces.json" \
  && ok "uninstall: the plugin index points at ~/.claude/plugins again" || { fail "uninstall: plugin index still spelled through the profile"; cat "$d/home/.claude/plugins/known_marketplaces.json" }
[[ -f "$cfg/work.conf" && -f "$cfg/home.conf" && -d "$d/home/.local/state/ccmorph" ]] \
  && ok "uninstall: state root and usage receipts are left in place" || fail "uninstall: deleted user state"
grep -qF "$cfg  (2 profiles," "$d/out" && ok "uninstall: names the state root and counts its profiles" || { fail "uninstall: state root not named"; cat "$d/out" }
grep -qF "$d/home/.local/state/ccmorph  (usage receipts)" "$d/out" && ok "uninstall: names the usage receipts" || { fail "uninstall: usage dir not named"; cat "$d/out" }
grep -qxF "  rm -rf $cfg $d/home/.local/state/ccmorph" "$d/out" && ok "uninstall: prints the rm for both" || { fail "uninstall: rm line wrong"; cat "$d/out" }
grep -qxF "  security delete-generic-password -s ccmorph -a work.api_key" "$d/out" \
  && ok "uninstall: prints the keychain delete for each ref in the confs" || { fail "uninstall: keychain line missing"; cat "$d/out" }
(( $(grep -c 'security delete-generic-password' "$d/out") == 1 )) && ok "uninstall: one keychain line per ref, none for a plain conf" || fail "uninstall: keychain line count"
# No usage dir and a Linux uname: neither line appears, and the rm names one path.
# The uname is stubbed, not assumed: on a macOS runner the real one says Darwin.
d=$(fresh_env uninstall_bare)
run_installer "$d" >/dev/null
printf 'type=apikey\napi_key=keychain:work.api_key\n' > "$d/home/.config/ccmorph/work.conf"
printf '#!/bin/sh\necho Linux\n' > "$d/bin/uname"; chmod +x "$d/bin/uname"
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" \
  zsh "$REPO/install.sh" --uninstall >"$d/out" 2>&1
grep -qxF "  rm -rf $d/home/.config/ccmorph" "$d/out" && ! grep -q 'usage receipts\|keychain' "$d/out" \
  && ok "uninstall: lists only what exists (no usage dir, no keychain off macOS)" || { fail "uninstall: listed residue that is not there"; cat "$d/out" }
# macOS with no keychain reference in any conf: the grep finds nothing, and that
# must not end the uninstall mid-report (it did, under set -e).
d=$(fresh_env uninstall_darwin_plain)
run_installer "$d" >/dev/null
printf 'type=token\ntoken=canary\n' > "$d/home/.config/ccmorph/work.conf"
printf '#!/bin/sh\necho Darwin\n' > "$d/bin/uname"; chmod +x "$d/bin/uname"
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" \
  zsh "$REPO/install.sh" --uninstall >"$d/out" 2>&1; rc=$?
(( rc == 0 )) && grep -qF 'profiles' "$d/out" && grep -qxF "  rm -rf $d/home/.config/ccmorph" "$d/out" && ! grep -q keychain "$d/out" \
  && ok "uninstall: macOS with no keychain refs still reports in full and exits 0" || { fail "uninstall: macOS without keychain refs rc=$rc"; cat "$d/out" }
fi

# --- 5b. CCMORPH_STATE_DIR moves the profiles: the report names the redirected root
if _own statedir; then
# The runtime derives the state root from CCMORPH_STATE_DIR (lib/ccmorph.sh), so
# on a host that sets it the confs, credentials and profile homes live under the
# redirected root, while ~/.config/ccmorph holds only the installed code. The
# uninstall names what it leaves: the redirected root as the state root (its
# profiles counted there, its keychain refs read from there, the rm spelled for
# it), the code dir as code — never as the profiles' home — and the plugin
# repair walks the redirected profile homes (#143).
d=$(fresh_env uninstall_state_dir)
run_installer "$d" >/dev/null
cfg="$d/home/.config/ccmorph"          # what the install wrote: the code
state="$d/moved-state"                 # where the runtime keeps the profiles
mkdir -p "$state"
printf 'type=apikey\napi_key=keychain:team.api_key\n' > "$state/team.conf"
printf 'type=sub\n' > "$state/home.conf"
mkdir -p "$d/home/.claude/plugins/marketplaces/example"
# A plugin recorded under the profile's spelling of the shared store, through
# the REDIRECTED profile home — the path that dangles once the state root goes.
print -r -- '{"example": {"source": {"source": "github", "repo": "owner/repo"}, "installLocation": "'"$state"'/profiles/team/plugins/marketplaces/example"}}' \
  > "$d/home/.claude/plugins/known_marketplaces.json"
printf '#!/bin/sh\necho Darwin\n' > "$d/bin/uname"; chmod +x "$d/bin/uname"
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" CCMORPH_STATE_DIR="$state" \
  zsh "$REPO/install.sh" --uninstall >"$d/out" 2>&1; rc=$?
(( rc == 0 )) && ok "state-dir uninstall: exits 0" || { fail "state-dir uninstall rc=$rc"; cat "$d/out" }
[[ -f "$state/team.conf" && -f "$state/home.conf" ]] \
  && ok "state-dir uninstall: leaves the redirected state in place" || fail "state-dir uninstall: deleted redirected state"
grep -qF "$state  (2 profiles," "$d/out" \
  && ok "state-dir uninstall: names the redirected root and counts its profiles" || { fail "state-dir uninstall: redirected root not named"; cat "$d/out" }
grep -qxF "  $cfg  (the installed code)" "$d/out" \
  && ok "state-dir uninstall: names the code dir as code, not as the profiles' home" || { fail "state-dir uninstall: code dir line wrong"; cat "$d/out" }
grep -qxF "  rm -rf $state $cfg" "$d/out" \
  && ok "state-dir uninstall: the rm names both roots" || { fail "state-dir uninstall: rm line wrong"; cat "$d/out" }
grep -qxF "  security delete-generic-password -s ccmorph -a team.api_key" "$d/out" \
  && ok "state-dir uninstall: keychain refs are read off the redirected confs" || { fail "state-dir uninstall: keychain line missing"; cat "$d/out" }
grep -qF "\"$d/home/.claude/plugins/marketplaces/example\"" "$d/home/.claude/plugins/known_marketplaces.json" \
  && ok "state-dir uninstall: the plugin repair walks the redirected profile homes" \
  || { fail "state-dir uninstall: plugin index still spelled through the profile"; cat "$d/home/.claude/plugins/known_marketplaces.json" }

# --- 5c. the redirect came AFTER the profiles: the code dir holds them too (#165)
# A later CCMORPH_STATE_DIR moves nothing (docs: Where state lives), so at
# uninstall team.conf, its keychain ref and its profile home are still under
# ~/.config/ccmorph, beside the code the install wrote. The report must count
# and name them as kept user data — never as code alone — read their keychain
# refs, and offer the directory under that honest name: following the printed
# cleanup must never delete a profile the report did not name — and the plugin
# repair must walk those pre-override profile homes too, or the index still
# names plugin files the printed rm deletes (#187).
d=$(fresh_env uninstall_prior_profiles)
run_installer "$d" >/dev/null
cfg="$d/home/.config/ccmorph"          # the code, and the pre-override profiles
state="$d/moved-state"                 # where the runtime looks now
printf 'type=apikey\napi_key=keychain:team.api_key\n' > "$cfg/team.conf"
mkdir -p "$cfg/profiles/team" "$state"
# A plugin installed from that pre-override profile session, recorded under the
# profile's spelling of the shared store — through the CODE DIR's profiles
# root, the path the rm line below deletes.
mkdir -p "$d/home/.claude/plugins/marketplaces/example"
print -r -- '{"example": {"source": {"source": "github", "repo": "owner/repo"}, "installLocation": "'"$cfg"'/profiles/team/plugins/marketplaces/example"}}' \
  > "$d/home/.claude/plugins/known_marketplaces.json"
printf '#!/bin/sh\necho Darwin\n' > "$d/bin/uname"; chmod +x "$d/bin/uname"
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" CCMORPH_STATE_DIR="$state" \
  zsh "$REPO/install.sh" --uninstall >"$d/out" 2>&1; rc=$?
(( rc == 0 )) && ok "prior-profiles uninstall: exits 0" || { fail "prior-profiles uninstall rc=$rc"; cat "$d/out" }
[[ -f "$cfg/team.conf" && -d "$cfg/profiles/team" ]] \
  && ok "prior-profiles uninstall: leaves the pre-override profile in place" || fail "prior-profiles uninstall: deleted the pre-override profile"
grep -qF "$state  (0 profiles," "$d/out" \
  && ok "prior-profiles uninstall: names the redirected root with its own count" || { fail "prior-profiles uninstall: redirected root not named"; cat "$d/out" }
grep -qF "$cfg  (the installed code, and 1 profile(s), their credentials and session history" "$d/out" \
  && ok "prior-profiles uninstall: counts and names the pre-override profiles as user data, never as code alone" \
  || { fail "prior-profiles uninstall: code dir named as code alone"; cat "$d/out" }
grep -qxF "  security delete-generic-password -s ccmorph -a team.api_key" "$d/out" \
  && ok "prior-profiles uninstall: names the pre-override profile's keychain item" || { fail "prior-profiles uninstall: keychain line missing"; cat "$d/out" }
grep -qxF "  rm -rf $state $cfg" "$d/out" \
  && ok "prior-profiles uninstall: the rm names both roots under the honest naming above" \
  || { fail "prior-profiles uninstall: rm line wrong"; cat "$d/out" }
grep -qF "\"$d/home/.claude/plugins/marketplaces/example\"" "$d/home/.claude/plugins/known_marketplaces.json" \
  && ok "prior-profiles uninstall: the plugin repair walks the pre-override profile homes too" \
  || { fail "prior-profiles uninstall: plugin index still spelled through the pre-override profile"; cat "$d/home/.claude/plugins/known_marketplaces.json" }

fi

# --- 5d. an EQUIVALENT spelling of the code dir is not a redirect (#187)
if _own linkspell; then

# "Is the state root elsewhere?" is a question about directories, not strings
# (the rule the copy step applies to CONFIG_REAL): a CCMORPH_STATE_DIR that
# spells the config dir with a trailing slash, through a symlinked parent, or
# as a symlink itself names ONE directory. A string compare counted it as a
# redirect, so the same profiles, keychain refs and rm targets were printed
# twice. The report then names and rms the config dir's own spelling: rm on a
# symlink removes only the link, so a state root that IS one must never be the
# spelling the printed cleanup carries.
for spell in slash link leaf; do
  d=$(fresh_env "uninstall_equiv_$spell")
  run_installer "$d" >/dev/null
  cfg="$d/home/.config/ccmorph"
  state="$cfg/"                        # slash: the same directory, trailing /
  if [[ "$spell" == link ]]; then      # link: the same directory via a symlinked parent
    ln -s "$d/home" "$d/home-link"
    state="$d/home-link/.config/ccmorph"
  fi
  if [[ "$spell" == leaf ]]; then      # leaf: the state root itself is a symlink
    ln -s "$cfg" "$d/state-link"
    state="$d/state-link"
  fi
  printf 'type=apikey\napi_key=keychain:team.api_key\n' > "$cfg/team.conf"
  mkdir -p "$cfg/profiles/team"
  # A plugin recorded under the PLAIN spelling of the same directory, from a
  # session that ran before the override took the other spelling: the repair
  # must walk that spelling too, or the entry dangles once the rm runs (#187).
  mkdir -p "$d/home/.claude/plugins/marketplaces/example"
  print -r -- '{"example": {"source": {"source": "github", "repo": "owner/repo"}, "installLocation": "'"$cfg"'/profiles/team/plugins/marketplaces/example"}}' \
    > "$d/home/.claude/plugins/known_marketplaces.json"
  printf '#!/bin/sh\necho Darwin\n' > "$d/bin/uname"; chmod +x "$d/bin/uname"
  env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" CCMORPH_STATE_DIR="$state" \
    zsh "$REPO/install.sh" --uninstall >"$d/out" 2>&1; rc=$?
  (( rc == 0 )) && ok "equiv-$spell uninstall: exits 0" || { fail "equiv-$spell uninstall rc=$rc"; cat "$d/out" }
  [[ -f "$cfg/team.conf" ]] \
    && ok "equiv-$spell uninstall: leaves the profile in place" || fail "equiv-$spell uninstall: deleted the profile"
  grep -qF "$cfg  (1 profiles," "$d/out" \
    && ok "equiv-$spell uninstall: names the one directory once, by its own spelling" \
    || { fail "equiv-$spell uninstall: state root line wrong"; cat "$d/out" }
  grep -qF 'the installed code' "$d/out" \
    && { fail "equiv-$spell uninstall: the same directory counted as a redirect"; cat "$d/out" } \
    || ok "equiv-$spell uninstall: an equivalent spelling is not a redirect"
  grep -qxF "  rm -rf $cfg" "$d/out" \
    && ok "equiv-$spell uninstall: the rm names the directory, never a link to it" \
    || { fail "equiv-$spell uninstall: rm line wrong"; cat "$d/out" }
  (( $(grep -c 'security delete-generic-password' "$d/out") == 1 )) \
    && ok "equiv-$spell uninstall: one keychain line for the one profile" \
    || { fail "equiv-$spell uninstall: keychain line count"; cat "$d/out" }
  grep -qF "\"$d/home/.claude/plugins/marketplaces/example\"" "$d/home/.claude/plugins/known_marketplaces.json" \
    && ok "equiv-$spell uninstall: the plugin repair walks the plain spelling too" \
    || { fail "equiv-$spell uninstall: plugin index still spelled through the plain spelling"; cat "$d/home/.claude/plugins/known_marketplaces.json" }
  # The printed cleanup, run as printed: on a spelling that is itself a link,
  # rm removes the link and leaves the directory behind — the report must hand
  # rm a path whose removal removes the data it names.
  rm_target="$(grep -F '  rm -rf ' "$d/out" | head -1)"
  rm_target="${rm_target#  rm -rf }"
  [[ -n "$rm_target" ]] && rm -rf "$rm_target"
  [[ ! -e "$cfg" ]] \
    && ok "equiv-$spell uninstall: the printed rm removes the directory" \
    || fail "equiv-$spell uninstall: the printed rm left the directory behind"
done

# --- 5e. a link's spelling never reaches the rm line (#207)
# rm on a symlink removes only the link, so the printed cleanup must carry the
# directory a link names, whichever root is the link: a redirected state root,
# the same with a trailing slash or through a second link, the config dir itself,
# a state root that links at a symlinked config dir, and the usage receipts.
for spell in state slash chain config equiv usage; do
  d=$(fresh_env "uninstall_rm_$spell")
  run_installer "$d" >/dev/null
  cfg="$d/home/.config/ccmorph"
  target="$d/elsewhere/data"           # where the link leads: the data under test
  mkdir -p "$target"
  link="$d/state-link"
  state_env=()
  case "$spell" in
    state|slash) ln -s "$target" "$link"; state_env=(CCMORPH_STATE_DIR="$link") ;;
    chain) ln -s "$target" "$d/hop"; ln -s "$d/hop" "$link"; state_env=(CCMORPH_STATE_DIR="$link") ;;
    config|equiv) rmdir "$target"; mv "$cfg" "$target"; ln -s "$target" "$cfg"; link="$cfg" ;;
    usage) link="$d/home/.local/state/ccmorph"; mkdir -p "${link:h}"; ln -s "$target" "$link" ;;
  esac
  [[ "$spell" == slash ]] && state_env=(CCMORPH_STATE_DIR="$link/")
  if [[ "$spell" == equiv ]]; then
    ln -s "$cfg" "$d/state-link"; state_env=(CCMORPH_STATE_DIR="$d/state-link")
  fi
  real="${target:A}"
  if [[ "$spell" == usage ]]; then
    mkdir -p "$real/usage"; label='(usage receipts)'
  else
    printf 'type=apikey\napi_key=keychain:team.api_key\n' > "$real/team.conf"
    mkdir -p "$real/profiles/team"; label='(1 profiles,'
    # A plugin recorded under the TARGET's spelling, from a session that ran on the
    # directory itself: it dangles once the printed rm deletes that directory.
    mkdir -p "$d/home/.claude/plugins/marketplaces/example"
    print -r -- '{"example": {"source": {"source": "github", "repo": "owner/repo"}, "installLocation": "'"$real"'/profiles/team/plugins/marketplaces/example"}}' \
      > "$d/home/.claude/plugins/known_marketplaces.json"
  fi
  env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" "${state_env[@]}" \
    zsh "$REPO/install.sh" --uninstall >"$d/out" 2>&1; rc=$?
  (( rc == 0 )) && ok "rm-$spell uninstall: exits 0" || { fail "rm-$spell uninstall rc=$rc"; cat "$d/out" }
  grep -qF "  $real  $label" "$d/out" \
    && ok "rm-$spell uninstall: the report names the directory the link leads to" \
    || { fail "rm-$spell uninstall: report line wrong"; cat "$d/out" }
  grep -qxF "$link is a symlink to $real and stays: it is yours to remove." "$d/out" \
    && ok "rm-$spell uninstall: the link is named beside the cleanup" \
    || { fail "rm-$spell uninstall: the link is named nowhere"; cat "$d/out" }
  if [[ "$spell" != usage ]]; then
    grep -qF "\"$d/home/.claude/plugins/marketplaces/example\"" "$d/home/.claude/plugins/known_marketplaces.json" \
      && ok "rm-$spell uninstall: the plugin repair walks the target's spelling" \
      || { fail "rm-$spell uninstall: plugin index still spelled through the target"; cat "$d/home/.claude/plugins/known_marketplaces.json" }
  fi
  rm_line="$(grep -F '  rm -rf ' "$d/out")"
  [[ "$rm_line" != *"$link"* ]] \
    && ok "rm-$spell uninstall: the rm line never carries the link" \
    || { fail "rm-$spell uninstall: the rm line carries the link: $rm_line" }
  # The printed cleanup, run exactly as printed.
  eval "$rm_line"
  [[ ! -e "$target" ]] \
    && ok "rm-$spell uninstall: the printed rm removes the data" \
    || fail "rm-$spell uninstall: the printed rm left $target behind"
done

# A link whose target cannot be reached (the volume is unmounted) has only itself
# to remove: the report says where it led, so nothing there is left named nowhere.
# The caller's spelling never reaches the rm line — `rm -rf` on a dangling link
# spelled with a trailing slash exits 0 and leaves the link — and a readlink
# that fails must not end the uninstall before the report (#219).
for dspell in plain slash broken; do
  d=$(fresh_env "uninstall_rm_dangling_$dspell")
  run_installer "$d" >/dev/null
  ln -s "$d/unmounted/state" "$d/state-link"
  dstate="$d/state-link"
  [[ "$dspell" == slash ]] && dstate="$dstate/"
  if [[ "$dspell" == broken ]]; then
    # A readlink that fails: the link is still one the rm must remove, and the
    # report must still reach the end.
    printf '#!/bin/sh\nexit 1\n' > "$d/bin/readlink"; chmod +x "$d/bin/readlink"
  fi
  want="$d/unmounted/state"
  [[ "$dspell" == broken ]] && want="an unreadable target"
  env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" CCMORPH_STATE_DIR="$dstate" \
    zsh "$REPO/install.sh" --uninstall >"$d/out" 2>&1; rc=$?
  (( rc == 0 )) && grep -qF "$d/state-link is a symlink to $want, which does not exist now" "$d/out" \
    && ok "dangling-$dspell uninstall: names where the link led and exits 0" \
    || { fail "dangling-$dspell uninstall rc=$rc"; cat "$d/out" }
  grep -qF "To remove those too:" "$d/out" \
    && ok "dangling-$dspell uninstall: prints the report in full" \
    || { fail "dangling-$dspell uninstall: report cut short"; cat "$d/out" }
  rm_line="$(grep -F '  rm -rf ' "$d/out")"
  eval "$rm_line"
  [[ ! -L "$d/state-link" ]] \
    && ok "dangling-$dspell uninstall: the printed rm removes the link" \
    || { fail "dangling-$dspell uninstall: the printed rm left the link behind"; cat "$d/out" }
done

# A dangling usage-receipts link is named on the report and removed by the
# printed cleanup like a dangling state root: a path that tests as a symlink and
# not as a directory is still one the uninstall must name (#219).
d=$(fresh_env uninstall_usage_dangling)
run_installer "$d" >/dev/null
link="$d/home/.local/state/ccmorph"
mkdir -p "${link:h}"
ln -s "$d/unmounted/usage" "$link"
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" \
  zsh "$REPO/install.sh" --uninstall >"$d/out" 2>&1; rc=$?
(( rc == 0 )) && grep -qxF "  $link  (usage receipts)" "$d/out" \
  && ok "dangling-usage uninstall: names the usage link on the report" \
  || { fail "dangling-usage uninstall rc=$rc"; cat "$d/out" }
grep -qF "$link is a symlink to $d/unmounted/usage, which does not exist now" "$d/out" \
  && ok "dangling-usage uninstall: says where the link led" \
  || { fail "dangling-usage uninstall: link named nowhere"; cat "$d/out" }
rm_line="$(grep -F '  rm -rf ' "$d/out")"
eval "$rm_line"
[[ ! -L "$link" ]] \
  && ok "dangling-usage uninstall: the printed rm removes the link" \
  || { fail "dangling-usage uninstall: the printed rm left the link behind"; cat "$d/out" }

# A link whose target cannot be asked is not a dangling link (the #204 rule):
# a mount answering EACCES here, a stalled volume in the field, means nothing in
# it could be read. The report must say so, count no profiles as fact, name the
# target unread on the rm line, and leave the link (#219).
d=$(fresh_env uninstall_rm_unreached)
run_installer "$d" >/dev/null
data="$d/locked/state"
mkdir -p "$data/profiles/team"
print -r -- 'type=apikey' > "$data/team.conf"
ln -s "$data" "$d/state-link"
chmod 000 "$d/locked"
if [[ -r "$d/locked" ]]; then
  # Running as root reads through mode 000 and the stat cannot be made to fail.
  chmod 755 "$d/locked"
  ok "unreached-link uninstall: not run, this user reads through mode 000"
else
  env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" CCMORPH_STATE_DIR="$d/state-link" \
    zsh "$REPO/install.sh" --uninstall >"$d/out" 2>&1; rc=$?
  chmod 755 "$d/locked"   # before any assert: the case dir must stay deletable
  (( rc == 0 )) && ok "unreached-link uninstall: exits 0" || { fail "unreached-link uninstall rc=$rc"; cat "$d/out" }
  grep -qxF "$d/state-link is a symlink to $data, which could not be reached (EACCES): nothing in it could be read or counted above, and the rm names it unread; the link stays: it is yours to remove." "$d/out" \
    && ok "unreached-link uninstall: says the target could not be reached" \
    || { fail "unreached-link uninstall: report line wrong"; cat "$d/out" }
  grep -qF "does not exist now" "$d/out" \
    && { fail "unreached-link uninstall: reported as dangling"; cat "$d/out" } \
    || ok "unreached-link uninstall: not reported as dangling"
  grep -qF "0 profiles" "$d/out" \
    && { fail "unreached-link uninstall: counted profiles it could not read"; cat "$d/out" } \
    || ok "unreached-link uninstall: counts no profiles it could not read"
  grep -qxF "  $data  (not read, so its profiles, credentials and session history are not counted)" "$d/out" \
    && ok "unreached-link uninstall: names the target as unread" \
    || { fail "unreached-link uninstall: root line wrong"; cat "$d/out" }
  rm_line="$(grep -F '  rm -rf ' "$d/out")"
  [[ "$rm_line" == *" $data"* && "$rm_line" != *"$d/state-link"* ]] \
    && ok "unreached-link uninstall: the rm names the target, never the link" \
    || { fail "unreached-link uninstall: rm line wrong: $rm_line"; cat "$d/out" }
  eval "$rm_line"
  [[ ! -e "$data" ]] \
    && ok "unreached-link uninstall: once the target answers, the printed rm removes it" \
    || fail "unreached-link uninstall: the printed rm left $data behind"
fi

fi

# --- 6. a RE-RUN is an update: user state survives, and it says so ----------
if _own rerun; then
# The guarantee people actually care about. Profiles (which now carry their own
# probed/manual context alongside credentials) and local.sh are never in
# COMPONENTS and are gitignored, so they were always safe — but "safe by
# construction" is exactly the kind of property that a later refactor breaks
# silently, and a re-run that looks destructive is one nobody runs twice.
d=$(fresh_env update)
run_installer "$d" >/dev/null
cfg="$d/home/.config/ccmorph"

# User state, as a real install would hold it. Probed/manual context now lives
# INSIDE the owning profile's own conf (context_discovered / context_window.*),
# not in a separate registry file — a re-run must leave that line alone too.
print 'type=apikey\napi_key=sk_test\nbase_url=https://example.invalid\nadapter=true\ncontext_window.claude-opus-5=1000000' > "$cfg/work.conf"
print 'type=sub'                                   > "$cfg/personal.conf"
print 'export CCMORPH_TEST_LOCAL=1'               > "$cfg/local.sh"
print 'active-marker'                              > "$cfg/active"

# The pre-Go-port layout: v0.2's installer placed the four Python helpers in
# libexec/ beside the shim. A session it launched keeps naming
# libexec/ccmorph-adapter in its route spec for a mid-session /model (the shim
# execs it for --ensure-config), so an upgrade that deletes that file breaks
# the switch under the live session (#52).
for h in ccmorph-adapter model-window settings-compose refresh-model-registry gateway-shim; do
  print -r -- "#!/usr/bin/env python3\n# v2-marker-$h" > "$cfg/libexec/$h"
  chmod +x "$cfg/libexec/$h"
done

# An update must not write into the installed shim's file: overwriting a signed Mach-O
# in place leaves a stale code-signature cache and macOS kills it on launch (exit 137).
# A hardlink keeps the old file reachable: it still holds the marker only if the
# installer replaced the shim instead of rewriting it (inode numbers can be reused).
print -n MARK >> "$cfg/libexec/ccmorph-core"
ln "$cfg/libexec/ccmorph-core" "$cfg/old-shim"
run_installer "$d"; rc=$?
[[ "$(tail -c 4 "$cfg/old-shim")" == MARK && "$(tail -c 4 "$cfg/libexec/ccmorph-core")" != MARK ]] \
  && ok "re-run replaces ccmorph-core instead of overwriting it" || fail "re-run overwrote ccmorph-core in place"
rm -f "$cfg/old-shim"
[[ $rc -eq 0 ]] && ok "re-run exits 0" || { fail "re-run rc=$rc"; cat "$d/out" }

# The adapter a running session names survives the upgrade, still executable
# and still the old copy (the new code never writes it): deleting it is what
# turned a live session's /model into a refusal about gateway availability.
[[ -x "$cfg/libexec/ccmorph-adapter" ]] && grep -qF 'v2-marker-ccmorph-adapter' "$cfg/libexec/ccmorph-adapter" \
  && ok "upgrade keeps libexec/ccmorph-adapter for sessions that still run it" \
  || fail "upgrade deleted or replaced libexec/ccmorph-adapter under a live session (#52)"
# The other three retired helpers are dead weight no running process names:
# they are not in any route spec and nothing execs them mid-session.
retired_left=()
for h in model-window settings-compose refresh-model-registry gateway-shim; do
  [[ -e "$cfg/libexec/$h" ]] && retired_left+=("$h")
done
(( ${#retired_left} == 0 )) \
  && ok "upgrade still clears the helpers nothing running names" \
  || fail "upgrade left retired helpers behind: ${retired_left[*]}"

# Every piece of user state survives byte-for-byte.
for f in work.conf personal.conf local.sh active; do
  [[ -f "$cfg/$f" ]] || { fail "re-run destroyed $f"; continue }
done
grep -qF 'sk_test'  "$cfg/work.conf"     && ok "re-run preserved profile credentials" || fail "profile clobbered"
grep -qF 'context_window.claude-opus-5=1000000' "$cfg/work.conf" \
  && ok "re-run preserved the profile's own manual context" || fail "profile's context_window entry clobbered"
grep -qF 'CCMORPH_TEST_LOCAL' "$cfg/local.sh" && ok "re-run preserved local.sh"       || fail "local.sh clobbered"

# And it REPORTS an update rather than claiming a fresh install.
grep -qiF 'fresh install' "$d/out" && fail "re-run still claims 'fresh install'" || ok "re-run does not claim fresh install"
grep -qiE 'reinstalled in place|Updated ccmorph' "$d/out" && ok "re-run reports an update" || { fail "re-run did not report an update"; cat "$d/out" }
grep -qiF 'Preserved:' "$d/out" && ok "re-run names what it preserved" || fail "re-run did not name preserved state"

# And a re-run still leaves the rc alone.
grep -qF '# >>> ccmorph >>>' "$d/home/.zshrc" \
  && fail "re-run wired the rc" || ok "re-run leaves the rc alone"
fi

# --- 7. missing optional deps warn, and never install unattended ------------
if _own optdeps; then
# The offer must degrade to a decline when there is no tty: a piped or CI run
# must not block, and must not `brew install` behind the operator's back.
d=$(fresh_env optdeps)
rm -f "$d/bin/fzf"
# A hermetic PATH: the default one carries /opt/homebrew/bin, where this very
# machine has a real cliproxyapi and fzf — so the case would silently test
# "deps present" instead of "deps absent". A `brew` stub IS provided, so the
# offer path is exercised rather than short-circuited by a brew-less host; it
# exits non-zero to prove a declined offer never runs it.
printf '#!/bin/sh\necho "STUB BREW CALLED" >&2\nexit 1\n' > "$d/bin/brew"
chmod +x "$d/bin/brew"
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" \
  zsh "$REPO/install.sh" >"$d/out" 2>&1 </dev/null
rc=$?
[[ $rc -eq 0 ]] && ok "missing optional deps do not fail the install" || { fail "optional dep aborted install rc=$rc"; cat "$d/out" }
grep -qF 'cliproxyapi' "$d/out" && ok "absent translator is named" || fail "absent translator not mentioned"
# The note a person on a FIRST install reads: what the translator is FOR, in
# plain words, and that nothing else needs it — never `adapter=true`, a setting
# someone with no profile yet has never seen (#273).
grep -qxF 'note: cliproxyapi not found.' "$d/out" \
  && ok "the translator note headline carries no parenthetical label" || { fail "the translator note headline still has a label"; cat "$d/out" }
grep -qF 'needed only to use non-Anthropic models' "$d/out" && grep -qF 'from a gateway with Claude Code' "$d/out" \
  && ok "the translator note says what the tool is for" || { fail "the translator note does not say what the tool is for"; cat "$d/out" }
grep -qF 'Nothing else needs it' "$d/out" \
  && ok "the translator note says nothing else needs it" || { fail "the note does not say nothing else needs it"; cat "$d/out" }
grep -qF 'adapter=true' "$d/out" \
  && { fail "the translator note still names the adapter=true setting"; cat "$d/out" } \
  || ok "the translator note names no profile setting"
grep -qF 'fzf'         "$d/out" && ok "absent fzf is named"        || fail "absent fzf not mentioned"
grep -qiF 'non-interactive' "$d/out" && ok "non-interactive offer declines loudly" || { fail "offer did not declare itself skipped"; cat "$d/out" }
grep -qF 'STUB BREW CALLED' "$d/out" && fail "declined offer still ran brew" || ok "declined offer never ran brew"
fi

# --- 8. remote mode: run outside a source tree, install from a download -------
if _own remote; then
# `sh -c "$(curl -fsSL .../install.sh)"` and `curl ... | sh` start this script with
# $0 = sh and no repo beside it. The installer must fetch the tree and run ITS
# installer. No network here: a stub curl copies the tarball built in the
# preamble to the path after -o and records its argv.

# 8.1 sh -c "$(cat install.sh)": the documented one-liner shape.
d=$(remote_env remote_c "$TMP/src.tar.gz")
remote_run "$d" cmd; rc=$?
(( rc == 0 )) && ok "remote: sh -c \"\$(cat install.sh)\" exits 0" || { fail "remote sh -c rc=$rc"; cat "$d/out" "$d/err" }
[[ -f "$d/home/.config/ccmorph/lib/ccmorph.sh" ]] \
  && ok "remote: sh -c installs the library" || fail "remote sh -c did not install lib/ccmorph.sh"
[[ -x "$d/home/.local/bin/ccmorph" ]] \
  && ok "remote: sh -c installs the command" || fail "remote sh -c did not install ~/.local/bin/ccmorph"
grep -qxF "$url_release" "$d/curl.argv" 2>/dev/null \
  && ok "remote: default URL is the latest release, not main" || { fail "remote: curl did not get $url_release"; cat "$d/curl.argv" 2>&1 }
[[ -z "$(print -rl -- "$d"/tmp/ccmorph-install.*(N))" ]] \
  && ok "remote: the temp dir is removed after a successful install" || fail "remote: temp dir left behind"

# 8.2 the same through a pipe (stdin is the script).
d=$(remote_env remote_pipe "$TMP/src.tar.gz")
remote_run "$d" pipe; rc=$?
(( rc == 0 )) && ok "remote: cat install.sh | sh exits 0" || { fail "remote pipe rc=$rc"; cat "$d/out" "$d/err" }
[[ -f "$d/home/.config/ccmorph/lib/ccmorph.sh" && -x "$d/home/.local/bin/ccmorph" ]] \
  && ok "remote: pipe installs library and command" || fail "remote pipe did not install"
# ...and into zsh, which the re-exec block alone would not have covered. Run from
# the case dir: with $0 = zsh the old code took the CWD for the source tree.
d=$(remote_env remote_zsh "$TMP/src.tar.gz")
(cd "$d" && cat "$REPO/install.sh" | env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" TMPDIR="$d/tmp" \
  zsh >"$d/out" 2>"$d/err"); rc=$?
(( rc == 0 )) && [[ -f "$d/home/.config/ccmorph/lib/ccmorph.sh" ]] && grep -qxF "$url_release" "$d/curl.argv" 2>/dev/null \
  && ok "remote: cat install.sh | zsh downloads and installs" || { fail "remote zsh pipe rc=$rc"; cat "$d/out" "$d/err" }

# 8.3 CCMORPH_REF picks the archive.
d=$(remote_env remote_ref "$TMP/src.tar.gz")
remote_run "$d" cmd CCMORPH_REF=v9.9.9; rc=$?
grep -qxF 'https://github.com/akleizor/ccmorph/archive/v9.9.9.tar.gz' "$d/curl.argv" 2>/dev/null \
  && ok "remote: CCMORPH_REF selects the archive" || { fail "remote: CCMORPH_REF=v9.9.9 not in curl argv (rc=$rc)"; cat "$d/curl.argv" 2>&1 }

# 8.4 CCMORPH_INSTALL_URL overrides the whole URL, verbatim — over the release
# default and over a CCMORPH_REF archive alike.
d=$(remote_env remote_url "$TMP/src.tar.gz")
remote_run "$d" cmd CCMORPH_INSTALL_URL=file:///x/y.tgz; rc=$?
grep -qxF 'file:///x/y.tgz' "$d/curl.argv" 2>/dev/null \
  && ok "remote: CCMORPH_INSTALL_URL reaches curl exactly" || { fail "remote: CCMORPH_INSTALL_URL not in curl argv (rc=$rc)"; cat "$d/curl.argv" 2>&1 }
grep -qF 'releases/latest' "$d/curl.argv" 2>/dev/null \
  && fail "remote: the default URL was used alongside the override" || ok "remote: override replaces the release URL"
d=$(remote_env remote_url_ref "$TMP/src.tar.gz")
remote_run "$d" cmd CCMORPH_REF=v9.9.9 CCMORPH_INSTALL_URL=file:///x/y.tgz; rc=$?
grep -qxF 'file:///x/y.tgz' "$d/curl.argv" 2>/dev/null && ! grep -qF 'archive/v9.9.9' "$d/curl.argv" 2>/dev/null \
  && ok "remote: the override also replaces a CCMORPH_REF archive URL" \
  || { fail "remote: the CCMORPH_REF archive was used beside the override (rc=$rc)"; cat "$d/curl.argv" 2>&1 }

# 8.5 neither curl nor wget: say so, fail, leave nothing behind. PATH holds only
# symlinks to the tools the bootstrap needs, so the real curl/wget cannot be found.
d=$(fresh_env remote_nodl)
mkdir -p "$d/pbin" "$d/tmp"
for t in sh tar mktemp dirname rm mkdir cat uname; do ln -s "$(command -v $t)" "$d/pbin/$t"; done
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/pbin" TMPDIR="$d/tmp" \
  sh -c "$(cat "$REPO/install.sh")" >"$d/out" 2>"$d/err" </dev/null; rc=$?
(( rc == 1 )) && ok "remote: no curl and no wget exits 1" || { fail "remote nodl rc=$rc (want 1)"; cat "$d/out" "$d/err" }
grep -qF 'curl' "$d/err" && grep -qF 'wget' "$d/err" \
  && ok "remote: the error names both curl and wget" || { fail "remote nodl error does not name curl and wget"; cat "$d/err" }
[[ -z "$(print -rl -- "$d"/tmp/ccmorph-install.*(N))" ]] \
  && ok "remote: no temp dir left after a missing downloader" || fail "remote nodl left a temp dir"
[[ -z "$(print -rl -- "$d"/home/*(N) "$d"/home/.[a-z]*(N))" ]] \
  && ok "remote: nothing written under HOME when the download cannot start" || fail "remote nodl wrote under HOME"

# 8.6 a download that is not a ccmorph tree is refused, and cleaned up.
d=$(remote_env remote_bad "$TMP/empty.tar.gz")
remote_run "$d" cmd; rc=$?
(( rc == 1 )) && ok "remote: a non-ccmorph tarball exits 1" || { fail "remote bad tarball rc=$rc (want 1)"; cat "$d/out" "$d/err" }
grep -qiF 'not a ccmorph source tree' "$d/err" \
  && ok "remote: the error says it is not a ccmorph tree" || { fail "remote: wrong-tree error missing"; cat "$d/err" }
[[ -z "$(print -rl -- "$d"/tmp/ccmorph-install.*(N))" ]] \
  && ok "remote: no temp dir left after a refused tarball" || fail "remote bad tarball left a temp dir"
[[ ! -e "$d/home/.config/ccmorph" ]] \
  && ok "remote: a refused tarball installs nothing" || fail "remote bad tarball wrote ~/.config/ccmorph"

# 8.7 local mode is untouched: an install from a source tree never downloads.
d=$(remote_env remote_local "$TMP/src.tar.gz")
rm -f "$d/curl.argv"
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" TMPDIR="$d/tmp" \
  zsh "$REPO/install.sh" >"$d/out" 2>&1 </dev/null; rc=$?
(( rc == 0 )) && ok "remote: a local zsh install still exits 0" || { fail "local install rc=$rc"; cat "$d/out" }
[[ ! -e "$d/curl.argv" ]] && ok "remote: a local install never calls curl" || fail "local install called curl"
rm -f "$d/curl.argv"
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" TMPDIR="$d/tmp" \
  sh "$REPO/install.sh" >"$d/out" 2>&1 </dev/null; rc=$?
(( rc == 0 )) && [[ ! -e "$d/curl.argv" ]] \
  && ok "remote: a local sh install exits 0 and never calls curl" || { fail "local sh install rc=$rc or it called curl"; cat "$d/out" }

# 8.8 the default URL names a release asset, so make must build one: dist/
# ccmorph.tar.gz, a stable-name copy of the tag tarball. A release that attaches
# only dist/ccmorph-<tag>.tar.gz leaves the one-liner's default download a 404.
# Built on a throwaway tag in a scratch CLONE — tags are repo-global and shared
# with every other worktree, so the shared namespace is never touched — with the
# working tree's Makefile copied in: the recipe under test is the one beside this
# suite, while the archive content still comes from the tagged commit alone.
# `make release` itself would recurse into this suite (its `test` prerequisite),
# so this exercises the `dist` step release runs after verifying.
git clone -q "$REPO" "$TMP/relrepo"
cp "$REPO/Makefile" "$TMP/relrepo/Makefile"
reltag=v0.0.0-install-test
git -C "$TMP/relrepo" tag "$reltag"
make -C "$TMP/relrepo" dist TAG="$reltag" DIST="$TMP/reldist" >"$TMP/relmake.out" 2>&1; rc=$?
(( rc == 0 )) && ok "release: make dist exits 0 on a throwaway tag" \
  || { fail "release: make dist rc=$rc"; cat "$TMP/relmake.out" }
[[ -s "$TMP/reldist/ccmorph-$reltag.tar.gz" ]] \
  && ok "release: the tag-named tarball is built" || fail "release: dist/ccmorph-$reltag.tar.gz was not built"
cmp -s "$TMP/reldist/ccmorph-$reltag.tar.gz" "$TMP/reldist/ccmorph.tar.gz" \
  && ok "release: dist/ccmorph.tar.gz is byte-identical to the tag-named tarball" \
  || fail "release: dist/ccmorph.tar.gz is missing or differs from dist/ccmorph-$reltag.tar.gz"
fi

# --- 9. Windows shells are named and sent to WSL before anything else ----------
if _own windows; then
# Git Bash / MSYS / Cygwin have no zsh, and the failure they got was about zsh,
# not about Windows. A stub uname stands in for the platform; the message must
# come before remote mode (so no download happens) and before any write to HOME.
for plat in MINGW64_NT-10.0 MSYS_NT-10.0 CYGWIN_NT-10.0; do
  d=$(remote_env "windows_${plat%%_*}" "$TMP/src.tar.gz")
  printf '#!/bin/sh\necho %s\n' "$plat" > "$d/bin/uname"; chmod +x "$d/bin/uname"
  env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" TMPDIR="$d/tmp" \
    sh "$REPO/install.sh" >"$d/out" 2>"$d/err" </dev/null; rc=$?
  (( rc == 1 )) && ok "windows: $plat exits 1" || { fail "windows $plat rc=$rc (want 1)"; cat "$d/out" "$d/err" }
  grep -qF 'WSL' "$d/err" && ok "windows: $plat is pointed at WSL on stderr" || { fail "windows $plat: stderr does not mention WSL"; cat "$d/err" }
  [[ -z "$(print -rl -- "$d"/home/*(N) "$d"/home/.[a-z]*(N))" ]] \
    && ok "windows: $plat writes nothing under HOME" || fail "windows $plat wrote under HOME"
done
# Before remote mode: a piped/-c run on Windows must not reach for the network.
rm -f "$d/curl.argv"
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" TMPDIR="$d/tmp" \
  sh -c "$(cat "$REPO/install.sh")" >"$d/out" 2>"$d/err" </dev/null; rc=$?
(( rc == 1 )) && grep -qF 'WSL' "$d/err" && [[ ! -e "$d/curl.argv" ]] \
  && ok "windows: the check runs before remote mode (no download)" || { fail "windows: remote mode ran first (rc=$rc)"; cat "$d/err" "$d/curl.argv" 2>&1 }
# Without the stub the same invocation still installs.
d=$(fresh_env windows_not)
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" \
  sh "$REPO/install.sh" >"$d/out" 2>&1 </dev/null; rc=$?
(( rc == 0 )) && [[ -x "$d/home/.local/bin/ccmorph" ]] \
  && ok "windows: a Linux/macOS uname still installs" || { fail "non-windows install rc=$rc"; cat "$d/out" }
fi

# --- 10. one install command for the package manager that is there -----------
if _own hint; then
# The zsh-missing message sits in the POSIX preamble, before any zsh exists, so its
# table is POSIX sh and is tested the same way: the function is cut out of
# install.sh (no process substitution: that is not POSIX and dash rejects it) and
# sourced by a bare sh. PATH holds ONLY the stubs, so the answer cannot depend on
# which package manager the machine running the suite happens to have.
d="$TMP/hint"; mkdir -p "$d"
sed -n '/^_ccm_pkg_hint()/,/^}/p' "$REPO/install.sh" > "$d/hint.sh"
[[ -s "$d/hint.sh" ]] && ok "hint: _ccm_pkg_hint is defined at column 0 in install.sh" || fail "hint: _ccm_pkg_hint not found in install.sh"
hint_for() {  # $1 = package, rest = package managers present (as stubs); prints the hint
  local pkg="$1"; shift
  rm -rf "$d/pbin"; mkdir -p "$d/pbin"
  for m in "$@"; do printf '#!/bin/sh\necho "STUB %s CALLED" >&2\nexit 1\n' "$m" > "$d/pbin/$m"; chmod +x "$d/pbin/$m"; done
  env -i PATH="$d/pbin" "$(command -v sh)" -c ". '$d/hint.sh'; _ccm_pkg_hint $pkg" 2>&1
}
[[ "$(hint_for zsh apt-get)" == 'sudo apt-get install -y zsh' ]] && ok "hint: apt-get -> sudo apt-get install -y zsh" || fail "hint: apt-get gave '$(hint_for zsh apt-get)'"
[[ "$(hint_for zsh dnf)"     == 'sudo dnf install -y zsh' ]]     && ok "hint: dnf -> sudo dnf install -y zsh"         || fail "hint: dnf gave '$(hint_for zsh dnf)'"
[[ "$(hint_for zsh apk)"     == 'sudo apk add zsh' ]]            && ok "hint: apk -> sudo apk add zsh"                || fail "hint: apk gave '$(hint_for zsh apk)'"
[[ "$(hint_for zsh pacman)"  == 'sudo pacman -S zsh' ]]          && ok "hint: pacman -> sudo pacman -S zsh"           || fail "hint: pacman gave '$(hint_for zsh pacman)'"
[[ "$(hint_for zsh zypper)"  == 'sudo zypper install -y zsh' ]]  && ok "hint: zypper -> sudo zypper install -y zsh"   || fail "hint: zypper gave '$(hint_for zsh zypper)'"
[[ "$(hint_for zsh brew)"    == 'brew install zsh' ]]            && ok "hint: brew -> brew install zsh (no sudo)"     || fail "hint: brew gave '$(hint_for zsh brew)'"
[[ "$(hint_for fzf apt-get)" == 'sudo apt-get install -y fzf' ]] && ok "hint: the package name is the argument"       || fail "hint: fzf gave '$(hint_for fzf apt-get)'"
[[ -z "$(hint_for zsh)" ]] && ok "hint: no package manager -> prints nothing" || fail "hint: invented a command with no manager: '$(hint_for zsh)'"
# First found wins, in the order brew, apt-get, dnf, apk, pacman, zypper.
[[ "$(hint_for zsh zypper pacman apk dnf apt-get brew)" == 'brew install zsh' ]] \
  && ok "hint: brew outranks every other manager" || fail "hint: order wrong with everything present"
[[ "$(hint_for zsh zypper pacman apk dnf apt-get)" == 'sudo apt-get install -y zsh' ]] \
  && ok "hint: apt-get outranks dnf, apk, pacman, zypper" || fail "hint: apt-get did not win"
[[ "$(hint_for zsh zypper pacman apk dnf)" == 'sudo dnf install -y zsh' ]] \
  && ok "hint: dnf outranks apk, pacman, zypper" || fail "hint: dnf did not win"
[[ "$(hint_for zsh zypper pacman)" == 'sudo pacman -S zsh' ]] \
  && ok "hint: pacman outranks zypper" || fail "hint: pacman did not win"
[[ "$(hint_for zsh apt-get)" != *'STUB'* ]] && ok "hint: a stub manager is only detected, never run" || fail "hint: ran a package manager"

# The same table, end to end: a PATH with a shell and no zsh. The installer must
# print ONE command, run nothing, and write nothing.
nozsh_run() {  # $1 = case dir, rest = package managers present
  local d="$1"; shift
  rm -rf "$d/pbin"; mkdir -p "$d/pbin" "$d/home"
  for t in sh dirname uname; do ln -s "$(command -v $t)" "$d/pbin/$t"; done
  for m in "$@"; do printf '#!/bin/sh\necho "STUB %s CALLED" >&2\nexit 1\n' "$m" > "$d/pbin/$m"; chmod +x "$d/pbin/$m"; done
  env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/pbin" "$(command -v sh)" "$REPO/install.sh" >"$d/out" 2>"$d/err" </dev/null
}
d="$TMP/nozsh_dnf"; nozsh_run "$d" dnf; rc=$?
(( rc == 1 )) && ok "no zsh: exits 1" || fail "no zsh: rc=$rc (want 1)"
grep -qxF '  sudo dnf install -y zsh' "$d/err" \
  && ok "no zsh: names the one command for the manager found" || { fail "no zsh: dnf command missing"; cat "$d/err"; }
grep -qF 'apt install zsh' "$d/err" && fail "no zsh: still prints the three-distro list beside a known manager" || ok "no zsh: a known manager gets one command, not three"
grep -qF 'STUB' "$d/err" "$d/out" && fail "no zsh: ran the package manager" || ok "no zsh: the command is printed, never run"
[[ -z "$(print -rl -- "$d"/home/*(N) "$d"/home/.[a-z]*(N))" ]] && ok "no zsh: nothing written under HOME" || fail "no zsh: wrote under HOME"
d="$TMP/nozsh_none"; nozsh_run "$d"; rc=$?
(( rc == 1 )) && grep -qF 'apt install zsh' "$d/err" && grep -qF 'dnf install zsh' "$d/err" && grep -qF 'apk add zsh' "$d/err" \
  && ok "no zsh: with no package manager the three-distro list is the fallback" || { fail "no zsh: fallback list missing (rc=$rc)"; cat "$d/err"; }
fi

# --- 11. optional deps are offered through any package manager; required ones named
# These runs need a PATH WITHOUT the host's own package managers (a Linux box has
if _own pkgmgr; then
# apt-get in /usr/bin, which would outrank a dnf stub). $TMP/sys links every tool
# in /usr/bin and /bin except the ones a case must control, so the installer still
# finds cp, grep, chmod and the rest, and the result does not depend on the host.
mkdir -p "$TMP/sys"
zmodload -F zsh/files b:ln 2>/dev/null
sys_excluded=(brew apt-get dnf apk pacman zypper fzf cliproxyapi claude curl wget)
for f in /usr/bin/*(N) /bin/*(N); do
  t=${f:t}
  (( ${sys_excluded[(Ie)$t]} )) && continue
  [[ -e "$TMP/sys/$t" || -L "$TMP/sys/$t" ]] || ln -s "$f" "$TMP/sys/$t"
done
pm_env() {  # $1 = case, rest = package managers to stub; fzf, cliproxyapi absent
  local d; d=$(fresh_env "$1"); shift
  rm -f "$d/bin/fzf"
  for m in "$@"; do printf '#!/bin/sh\necho "STUB %s CALLED" >&2\nexit 1\n' "$m" > "$d/bin/$m"; chmod +x "$d/bin/$m"; done
  print -r -- "$d"
}
pm_run() {  # $1 = case dir
  env -i HOME="$1/home" ZDOTDIR="$1/home" PATH="$1/bin:$TMP/sys" \
    zsh "$REPO/install.sh" >"$1/out" 2>&1 </dev/null
}

for pm_case in "apt-get|sudo apt-get install -y fzf" "dnf|sudo dnf install -y fzf" "apk|sudo apk add fzf" \
               "pacman|sudo pacman -S fzf" "zypper|sudo zypper install -y fzf" "brew|brew install fzf"; do
  pm=${pm_case%%|*}; want_cmd=${pm_case#*|}
  d=$(pm_env "pm_$pm" $pm); pm_run "$d"; rc=$?
  (( rc == 0 )) && ok "$pm: a missing fzf does not fail the install" || { fail "$pm: install rc=$rc"; cat "$d/out"; }
  grep -qF "Run yourself: $want_cmd" "$d/out" \
    && ok "$pm: fzf is offered as '$want_cmd'" || { fail "$pm: no offer for '$want_cmd'"; cat "$d/out"; }
  grep -qF "STUB $pm CALLED" "$d/out" && fail "$pm: a declined offer still ran $pm" || ok "$pm: a declined offer never ran $pm"
done
d=$TMP/pm_apt-get
grep -qF 'install -y cliproxyapi' "$d/out" \
  && fail "apt-get: cliproxyapi offered through a manager that does not ship it" || ok "apt-get: cliproxyapi is not offered through apt-get"
grep -qF 'github.com/router-for-me/CLIProxyAPI' "$d/out" \
  && ok "apt-get: cliproxyapi still points at upstream" || fail "apt-get: the upstream link for cliproxyapi is gone"
d=$TMP/pm_brew
grep -qF 'Run yourself: brew install cliproxyapi' "$d/out" \
  && ok "brew: cliproxyapi is still offered through brew" || fail "brew: cliproxyapi offer lost"
grep -qF 'sudo' "$d/out" && fail "brew: a sudo appeared in a brew offer" || ok "brew: no sudo in a brew offer"

# No package manager at all: both notes appear, nothing is offered, install succeeds.
d=$(pm_env pm_none); pm_run "$d"; rc=$?
(( rc == 0 )) && grep -qF 'fzf not found' "$d/out" && ! grep -qF 'Run yourself' "$d/out" \
  && ok "no manager: fzf is named and no command is invented" || { fail "no manager: wrong output (rc=$rc)"; cat "$d/out"; }

# A missing claude is named with where to get it, never with a command; exit 1;
# nothing installed.
d=$(pm_env pm_noclaude apt-get); rm -f "$d/bin/claude"; pm_run "$d"; rc=$?
(( rc == 1 )) && ok "no claude: exits 1" || { fail "no claude: rc=$rc (want 1)"; cat "$d/out"; }
grep -qF -- "$claude_missing" "$d/out" \
  && ok "no claude: says the one sentence, with the setup link" || { fail "no claude: the sentence is missing"; cat "$d/out"; }
# The binary is searched with zsh itself, not grep: macOS grep -F silently
# fails to match a multibyte pattern inside a binary file when no UTF-8
# locale is in the environment, and the sentence carries an em dash.
if [[ "$(<"$REPO/libexec/ccmorph-core")" == *"$claude_missing"* ]]; then
  ok "no claude: installer and core binary carry the same sentence"
else
  fail "no claude: the core binary does not embed the installer's sentence"
fi
grep -qiE 'npm|curl|apt-get install -y claude|brew install claude' "$d/out" \
  && fail "no claude: embeds an install command for another vendor's product" || ok "no claude: no install command for it"
[[ ! -e "$d/home/.config/ccmorph" ]] && ok "no claude: nothing installed" || fail "no claude: wrote ~/.config/ccmorph"
grep -qF 'STUB' "$d/out" && fail "no claude: ran a package manager" || ok "no claude: nothing ran"

# python3 is not a dependency: a PATH without one installs, and the installer never names it.
d=$(pm_env pm_nopy dnf); pm_run "$d"; rc=$?
(( rc == 0 )) && ! grep -qiF 'python' "$d/out" && ! grep -qF 'STUB' "$d/out" \
  && ok "no python3: installs, and python3 is never named" || { fail "no python3: wrong result (rc=$rc)"; cat "$d/out"; }
fi

# --- 12. the PATH hint names the file and the syntax for the user's shell -----
if _own path; then
# ~/.local/bin is not on PATH in these runs (the fake PATH never carries it), so
# the hint fires. It must name the file and print a line the shell can use, and it
# must still never write to a startup file: three rc files are asserted absent.
path_run() {  # $1 = case, $2 = SHELL value ("" = unset), $3 = uname output ("" = real uname), $4 = ZDOTDIR override ("" = home)
  local d; d=$(fresh_env "$1")
  if [[ -n "$3" ]]; then printf '#!/bin/sh\necho %s\n' "$3" > "$d/bin/uname"; chmod +x "$d/bin/uname"; fi
  local -a sh_env; [[ -n "$2" ]] && sh_env=(SHELL="$2")
  env -i HOME="$d/home" ZDOTDIR="${4:-$d/home}" PATH="$d/bin:/usr/bin:/bin" "${sh_env[@]}" \
    zsh "$REPO/install.sh" >"$d/out" 2>&1 </dev/null
  print -r -- "$d"
}
no_rc_written() {  # $1 = case dir
  [[ ! -e "$1/home/.zshrc" && ! -e "$1/home/.bashrc" && ! -e "$1/home/.bash_profile" && ! -e "$1/home/.config/fish/config.fish" && ! -e "$1/zd/.zshrc" ]]
}

d=$(path_run path_fish /usr/bin/fish Linux)
grep -qF "fish_add_path $d/home/.local/bin" "$d/out" \
  && ok "path: fish gets fish_add_path <bin dir>" || { fail "path: fish hint missing"; cat "$d/out"; }
grep -qF "$d/home/.config/fish/config.fish" "$d/out" \
  && ok "path: fish names ~/.config/fish/config.fish" || fail "path: fish config file not named"
grep -qF 'export PATH=' "$d/out" && fail "path: fish was handed a bash-style export" || ok "path: fish is not handed an export line"
no_rc_written "$d" && ok "path: fish run edits no startup file" || fail "path: fish run wrote a startup file"

d=$(path_run path_bash_linux /bin/bash Linux)
grep -qF "$d/home/.bashrc" "$d/out" \
  && ok "path: bash on Linux names ~/.bashrc" || { fail "path: bash/Linux does not name .bashrc"; cat "$d/out"; }
grep -qF "export PATH=\"$d/home/.local/bin:\$PATH\"" "$d/out" \
  && ok "path: bash gets the export line" || fail "path: bash export line missing"
no_rc_written "$d" && ok "path: bash run edits no startup file" || fail "path: bash run wrote a startup file"

d=$(path_run path_bash_mac /bin/bash Darwin)
grep -qF "$d/home/.bash_profile" "$d/out" && ! grep -qF '.bashrc' "$d/out" \
  && ok "path: bash on macOS names ~/.bash_profile" || { fail "path: bash/Darwin does not name .bash_profile"; cat "$d/out"; }
no_rc_written "$d" && ok "path: macOS bash run edits no startup file" || fail "path: macOS bash run wrote a startup file"

d=$(path_run path_zsh /bin/zsh Linux)
grep -qF "$d/home/.zshrc" "$d/out" \
  && ok "path: zsh names ~/.zshrc" || { fail "path: zsh does not name .zshrc"; cat "$d/out"; }
grep -qF "export PATH=\"$d/home/.local/bin:\$PATH\"" "$d/out" \
  && ok "path: zsh gets the export line" || fail "path: zsh export line missing"
no_rc_written "$d" && ok "path: zsh run edits no startup file" || fail "path: zsh run wrote a startup file"

# ZDOTDIR moves the zsh file; an unknown or unset SHELL gets the zsh advice, which
# is what a bare `env -i` run (every test above this section) always printed.
d=$(path_run path_zdot /bin/zsh Linux "$TMP/path_zdot_zd")
grep -qF "$TMP/path_zdot_zd/.zshrc" "$d/out" \
  && ok "path: zsh honours ZDOTDIR in the file it names" || { fail "path: ZDOTDIR ignored"; cat "$d/out"; }
for sh_val in "" /usr/bin/tcsh; do
  key=${sh_val:t}; d=$(path_run "path_other_${key:-unset}" "$sh_val" Linux)
  grep -qF "$d/home/.zshrc" "$d/out" && grep -qF 'export PATH=' "$d/out" \
    && ok "path: SHELL='${sh_val:-unset}' falls back to the zsh advice" || { fail "path: SHELL='${sh_val:-unset}' got no zsh advice"; cat "$d/out"; }
done
fi

# --- 13. every run ends with the check and the next step ----------------------
if _own footer; then
# "Did it work, what now" has one answer at the end of every run: `ccmorph doctor`.
# A fresh install adds `ccmorph add <name>`; an update never does (it has profiles).
# When the bin dir is not on PATH the check is spelled with the absolute path, so
# it works before the shell is fixed, and the next step — still the bare command —
# is introduced by the line that says which terminal to run it in (#273). The old
# four-line "Start here" block is gone.
end_run() {  # $1 = case dir, $2 = "onpath" | "offpath"
  local extra=""; [[ "$2" == onpath ]] && extra="$1/home/.local/bin:"
  env -i HOME="$1/home" ZDOTDIR="$1/home" PATH="$extra$1/bin:/usr/bin:/bin" \
    zsh "$REPO/install.sh" >"$1/out" 2>&1 </dev/null
}

d=$(fresh_env end_fresh_on); end_run "$d" onpath; rc=$?
(( rc == 0 )) && ok "end: fresh install on PATH exits 0" || { fail "end: fresh on-PATH rc=$rc"; cat "$d/out"; }
grep -qxF 'Check:  ccmorph doctor' "$d/out" && ok "end: fresh install ends with 'Check:  ccmorph doctor'" || { fail "end: fresh install has no check line"; cat "$d/out"; }
grep -qxF 'Next:   ccmorph add <name>' "$d/out" && ok "end: fresh install says 'Next:   ccmorph add <name>'" || fail "end: fresh install has no next-step line"
grep -qE 'ccmorph (use|list)|Start here' "$d/out" && fail "end: the old Start here block is still printed" || ok "end: the old use/list block is gone"
grep -qF 'not on your PATH' "$d/out" && fail "end: on-PATH run still warns about PATH" || ok "end: no PATH warning when the bin dir is on PATH"
# On PATH the bare Next line runs in the terminal the person is already in, so
# gating it on a new terminal there would be wrong advice (#273).
grep -qF 'new terminal' "$d/out" && fail "end: on-PATH run gates the next step on a new terminal" || ok "end: no new-terminal line when the bin dir is on PATH"

d=$(fresh_env end_fresh_off); end_run "$d" offpath
grep -qxF "Check:  $d/home/.local/bin/ccmorph doctor" "$d/out" \
  && ok "end: off PATH the check uses the absolute path" || { fail "end: off-PATH check is not absolute"; cat "$d/out"; }
grep -qxF 'Next:   ccmorph add <name>' "$d/out" && ok "end: off PATH a fresh install still names the next step" || fail "end: off-PATH next-step line missing"
# That bare Next line cannot run in THIS terminal, so it must not stand alone: the
# line printed just above it has to send the person to the new terminal the PATH
# warning asked for (#273).
instr=$(awk 'p && $0 == "Next:   ccmorph add <name>" { print p; exit } { p = $0 }' "$d/out")
[[ "$instr" == *'new terminal'* ]] \
  && ok "end: off PATH the next step is shown after the new-terminal instruction" || { fail "end: off-PATH next step is not introduced by the new-terminal line"; cat "$d/out"; }
grep -qxF 'Check:  ccmorph doctor' "$d/out" && fail "end: off PATH the bare check was printed too" || ok "end: off PATH only the absolute check is printed"

d=$(fresh_env end_update_on); end_run "$d" onpath; end_run "$d" onpath; rc=$?
(( rc == 0 )) && ok "end: update on PATH exits 0" || fail "end: update rc=$rc"
grep -qxF 'Check:  ccmorph doctor' "$d/out" && ok "end: an update ends with the check" || { fail "end: update has no check line"; cat "$d/out"; }
grep -qF 'ccmorph add' "$d/out" && fail "end: an update suggests adding a profile" || ok "end: an update does not say 'ccmorph add'"
grep -qE '^Next:' "$d/out" && fail "end: an update prints a Next line" || ok "end: an update prints no Next line"
grep -qE 'ccmorph (use|list)|Start here' "$d/out" && fail "end: an update prints the old block" || ok "end: an update prints no use/list block"

d=$(fresh_env end_update_off); end_run "$d" offpath; end_run "$d" offpath
grep -qxF "Check:  $d/home/.local/bin/ccmorph doctor" "$d/out" && ! grep -qF 'ccmorph add' "$d/out" \
  && ok "end: an update off PATH uses the absolute check and no 'add'" || { fail "end: update off-PATH footer wrong"; cat "$d/out"; }
# Last thing printed: the check or the next step, not a stray trailing line.
last=$(grep -v '^$' "$d/out" | tail -1)
[[ "$last" == "Check:  "* ]] && ok "end: the check is the last line of an update" || fail "end: last line of an update is '$last'"
fi

# --- 14. `ccmorph update` and `ccmorph uninstall` run the INSTALLED installer ------
# The installer ships as a component, so the command can update itself with no clone
if _own selfupdate; then
# and no URL to remember: `update` hands the installed install.sh to remote mode
# (--remote), which downloads, unpacks and runs the downloaded installer. The same
# stub-curl harness as section 8; the tarball carries a different version, so the
# command's own --version proves the new code is the code that runs.
d=$(fresh_env shipped); run_installer "$d" >/dev/null
[[ -x "$d/home/.config/ccmorph/install.sh" ]] \
  && ok "update: a fresh install ships an executable install.sh" || fail "update: install.sh is not an executable component"

mkdir -p "$TMP/stage99"
cp -R "$TMP/stage/ccmorph-v1.2.3" "$TMP/stage99/ccmorph-main"
# The downloaded code is RUN here (update, --version, uninstall), so the 9.9.9
# tree carries the real binary, not the stub the section-8 tarball staged.
cp "$REPO/libexec/ccmorph-core" "$TMP/stage99/ccmorph-main/libexec/ccmorph-core"
sed 's/^_CCMORPH_VERSION=.*/_CCMORPH_VERSION="9.9.9"/' "$REPO/lib/ccmorph.sh" > "$TMP/stage99/ccmorph-main/lib/ccmorph.sh"
grep -qxF '_CCMORPH_VERSION="9.9.9"' "$TMP/stage99/ccmorph-main/lib/ccmorph.sh" || fail "update: could not stage the 9.9.9 tree"
tar -czf "$TMP/src99.tar.gz" -C "$TMP/stage99" ccmorph-main

# A release tree names its binary per platform, so no plain libexec/ccmorph-core
# sits beside the source lib; the install must still finish (#180 broke this).
mkdir -p "$TMP/stage-rel"
cp -R "$TMP/stage/ccmorph-v1.2.3" "$TMP/stage-rel/ccmorph-main"
rm "$TMP/stage-rel/ccmorph-main/libexec/ccmorph-core"
_rel_os="${$(uname -s):l}"; _rel_arch="$(uname -m)"
case "$_rel_arch" in x86_64) _rel_arch=amd64 ;; aarch64) _rel_arch=arm64 ;; esac
cp "$REPO/libexec/ccmorph-core" "$TMP/stage-rel/ccmorph-main/libexec/ccmorph-core-$_rel_os-$_rel_arch"
tar -czf "$TMP/src-rel.tar.gz" -C "$TMP/stage-rel" ccmorph-main
d=$(remote_env release-asset "$TMP/src-rel.tar.gz")
if remote_run "$d" cmd && [[ -x "$d/home/.config/ccmorph/libexec/ccmorph-core" ]]; then
  ok "remote: a tree with only per-platform binaries installs"
else
  fail "remote: a tree with only per-platform binaries did not install"
fi

upd_env() {  # $1 = case; an installed tree, a stub curl serving 9.9.9, and user state
  local d; d=$(remote_env "$1" "$TMP/src99.tar.gz")
  run_installer "$d" >/dev/null
  print 'type=token\ntoken=canary' > "$d/home/.config/ccmorph/work.conf"
  print 'export CCMORPH_TEST_LOCAL=1' > "$d/home/.config/ccmorph/local.sh"
  cp "$d/home/.config/ccmorph/work.conf" "$d/work.conf.before"
  cp "$d/home/.config/ccmorph/local.sh" "$d/local.sh.before"
  print -r -- "$d"
}
cmd_run() {  # $1 = case dir, rest = ccmorph arguments; the installed command, hermetic
  local d="$1"; shift
  rm -f "$d/curl.argv"
  env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" TMPDIR="$d/tmp" \
    "$d/home/.local/bin/ccmorph" "$@" >"$d/out" 2>"$d/err" </dev/null
}

# 14.1 update: no ref is the latest release (the one-liner's default), new code
# live, user state byte-identical.
d=$(upd_env upd_main); cmd_run "$d" update; rc=$?
(( rc == 0 )) && ok "update: exits 0" || { fail "update rc=$rc"; cat "$d/out" "$d/err" }
grep -qxF "$url_release" "$d/curl.argv" 2>/dev/null \
  && ok "update: no ref downloads the latest release" || { fail "update: curl argv lacks the latest release"; cat "$d/curl.argv" 2>&1 }
# An exported CCMORPH_REF does not redirect the bare command; only an argument does.
rm -f "$d/curl.argv"
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" TMPDIR="$d/tmp" CCMORPH_REF=v0.0.1 \
  "$d/home/.local/bin/ccmorph" update >/dev/null 2>&1 </dev/null
grep -qxF "$url_release" "$d/curl.argv" 2>/dev/null \
  && ok "update: an exported CCMORPH_REF does not change the bare update" || { fail "update: followed an exported CCMORPH_REF"; cat "$d/curl.argv" 2>&1 }
cmd_run "$d" update main
grep -qxF 'https://github.com/akleizor/ccmorph/archive/main.tar.gz' "$d/curl.argv" 2>/dev/null \
  && ok "update main: asks for the main archive" || { fail "update main: curl argv lacks archive/main.tar.gz"; cat "$d/curl.argv" 2>&1 }
cmd_run "$d" --version
[[ "$(<"$d/out")" == 'ccmorph 9.9.9' ]] \
  && ok "update: the command now reports the downloaded version" || { fail "update: --version prints '$(<"$d/out")'"; }
cmp -s "$d/work.conf.before" "$d/home/.config/ccmorph/work.conf" \
  && ok "update: a profile conf is byte-identical" || fail "update: work.conf changed"
cmp -s "$d/local.sh.before" "$d/home/.config/ccmorph/local.sh" \
  && ok "update: local.sh is byte-identical" || fail "update: local.sh changed"
[[ -x "$d/home/.config/ccmorph/install.sh" ]] \
  && ok "update: the installer is still installed afterwards" || fail "update: install.sh gone after an update"
[[ -z "$(print -rl -- "$d"/tmp/ccmorph-install.*(N))" ]] \
  && ok "update: no temp dir left behind" || fail "update: temp dir left behind"
# The report comes from the installer: it names the move and what survived.
cmd_run "$d" update
grep -qF 'Preserved:' "$d/out" && ok "update: says what it preserved" || { fail "update: no 'Preserved:' line"; cat "$d/out" "$d/err" }

# 14.2 update <ref>: a tag or a branch picks the archive.
d=$(upd_env upd_tag); cmd_run "$d" update v1.2.3; rc=$?
(( rc == 0 )) && ok "update v1.2.3: exits 0" || { fail "update v1.2.3 rc=$rc"; cat "$d/out" "$d/err" }
grep -qxF 'https://github.com/akleizor/ccmorph/archive/v1.2.3.tar.gz' "$d/curl.argv" 2>/dev/null \
  && ok "update v1.2.3: curl argv has archive/v1.2.3.tar.gz" || { fail "update v1.2.3: wrong archive"; cat "$d/curl.argv" 2>&1 }
cmd_run "$d" update feature/x-1.y_z
grep -qxF 'https://github.com/akleizor/ccmorph/archive/feature/x-1.y_z.tar.gz' "$d/curl.argv" 2>/dev/null \
  && ok "update: a branch name with a slash is accepted" || { fail "update: branch with slash refused"; cat "$d/out" "$d/err" }

# 14.3 a ref that is not a plain name is refused before anything is downloaded.
d=$(upd_env upd_badref)
for bad in 'x;rm' '../evil' 'a..b' '-rf' '/abs' 'a b' '$(id)' ''; do
  cmd_run "$d" update "$bad"; rc=$?
  (( rc != 0 )) && [[ ! -e "$d/curl.argv" ]] \
    && ok "update '$bad': refused, curl never called" || { fail "update '$bad': rc=$rc or curl ran"; cat "$d/curl.argv" 2>&1 }
done
grep -qi 'ref' "$d/err" && ok "update: a refused ref says what is wrong" || { fail "update: refusal does not mention the ref"; cat "$d/err" }
cmd_run "$d" update main extra; rc=$?
(( rc != 0 )) && [[ ! -e "$d/curl.argv" ]] \
  && ok "update: a second argument is refused, curl never called" || fail "update: accepted two arguments (rc=$rc)"
cmd_run "$d" uninstall now; rc=$?
(( rc != 0 )) && [[ -x "$d/home/.local/bin/ccmorph" ]] \
  && ok "uninstall: an argument is refused and nothing is removed" || fail "uninstall: accepted an argument (rc=$rc)"

# 14.4 a git checkout is updated with git: a download copied over it would dirty it.
d=$(upd_env upd_git); mkdir -p "$d/home/.config/ccmorph/.git"
cmd_run "$d" update; rc=$?
(( rc != 0 )) && [[ ! -e "$d/curl.argv" ]] \
  && ok "update: a git checkout is refused, curl never called" || { fail "update: ran on a git checkout (rc=$rc)"; cat "$d/curl.argv" 2>&1 }
grep -qi 'git' "$d/err" && ok "update: the refusal says to use git" || { fail "update: refusal does not mention git"; cat "$d/err" }
cmd_run "$d" --version
[[ "$(<"$d/out")" != 'ccmorph 9.9.9' ]] && ok "update: a refused update left the code alone" || fail "update: replaced a git checkout"

# 14.5 no installer beside the library (a packaged or older install): name the one-liner.
d=$(upd_env upd_noinst); rm -f "$d/home/.config/ccmorph/install.sh"
for verb in update uninstall; do
  cmd_run "$d" $verb; rc=$?
  (( rc == 1 )) && [[ ! -e "$d/curl.argv" ]] \
    && ok "$verb: no installer exits 1 and downloads nothing" || { fail "$verb: no-installer rc=$rc"; cat "$d/curl.argv" 2>&1 }
  grep -qF 'sh -c "$(curl -fsSL https://github.com/akleizor/ccmorph/releases/latest/download/install.sh)"' "$d/err" \
    && ok "$verb: no installer names the one-line install" || { fail "$verb: the one-line install is not named"; cat "$d/err" }
done
[[ -x "$d/home/.local/bin/ccmorph" ]] && ok "uninstall: a missing installer removes nothing" || fail "uninstall: removed the command without an installer"

# 14.6 uninstall: the command goes, the profiles stay, and it says so.
d=$(upd_env upd_rm); cmd_run "$d" uninstall; rc=$?
(( rc == 0 )) && ok "uninstall: exits 0" || { fail "uninstall rc=$rc"; cat "$d/out" "$d/err" }
[[ ! -e "$d/home/.local/bin/ccmorph" ]] && ok "uninstall: the command is gone from ~/.local/bin" || fail "uninstall: the command is still on PATH"
cmp -s "$d/work.conf.before" "$d/home/.config/ccmorph/work.conf" \
  && ok "uninstall: the profile is still there, byte-identical" || fail "uninstall: work.conf lost or changed"
grep -qF 'profiles' "$d/out" && ok "uninstall: says the profiles were kept" || { fail "uninstall: no word about profiles"; cat "$d/out" }
[[ ! -e "$d/curl.argv" ]] && ok "uninstall: never downloads" || fail "uninstall: called curl"

# 14.7 help: both verbs have their own entry, in the long list only.
d=$(upd_env upd_help)
cmd_run "$d" help all
grep -qE '^  ccmorph update \[ref\] +Update to the latest version, or to a branch or tag$' "$d/out" \
  && grep -qE '^  ccmorph uninstall +Remove the command; profiles stay$' "$d/out" \
  && ok "help all: lists update and uninstall under More commands" || { fail "help all: update/uninstall rows missing"; cat "$d/out" }
cmd_run "$d"
grep -qE 'ccmorph (update|uninstall)' "$d/out" \
  && fail "help: the short overview mentions update or uninstall" || ok "help: the short overview stays short"
for verb in update uninstall; do
  cmd_run "$d" help $verb; rc=$?
  (( rc == 0 )) && [[ "$(head -1 "$d/out")" == "Usage: ccmorph $verb"* ]] \
    && ok "help $verb: has its own usage" || { fail "help $verb: rc=$rc"; cat "$d/out" "$d/err" }
  cmd_run "$d" $verb --help; rc=$?
  (( rc == 0 )) && [[ "$(head -1 "$d/out")" == "Usage: ccmorph $verb"* ]] && [[ ! -e "$d/curl.argv" ]] \
    && ok "$verb --help: shows its help and runs nothing" || { fail "$verb --help: rc=$rc"; cat "$d/out" "$d/err" }
done
[[ -x "$d/home/.local/bin/ccmorph" ]] && ok "help: asking for help removed nothing" || fail "help: uninstall --help removed the command"
fi

# A shard reports its counts to the parent, which prints the one summary; a
# serial run prints it here, exactly as before.
if [[ -n "${CCMORPH_INSTALL_RUN_DIR:-}" ]]; then
  print -r -- "$passed $failed" > "$CCMORPH_INSTALL_RUN_DIR/count.$CCMORPH_INSTALL_SHARD"
else
  print ""
  print "install tests: $passed passed, $failed failed"
fi
(( failed == 0 ))
