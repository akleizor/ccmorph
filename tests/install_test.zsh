#!/usr/bin/env zsh
# install.sh tests — hermetic: fake HOME + ZDOTDIR under a mktemp dir.
# The installer checks for `claude` on PATH; a stub satisfies it.

set -u
emulate -L zsh

REPO="${0:A:h:h}"
# Derived, never hand-listed: install.sh enumerates this same glob, so a module
# added beside ccproteus.sh is covered here with no edit — and a module dropped
# from the repo cannot leave a stale name behind in the manifest below.
module_files=("$REPO"/ccproteus-*.zsh(N:t))   # not `modules`: a readonly zsh special
TMP=$(mktemp -d /tmp/ccproteus-install-test.XXXXXX)
trap 'rm -rf "$TMP"' EXIT

passed=0; failed=0
ok()   { print -r -- "ok: $1"; (( ++passed )) }
fail() { print -r -- "FAIL: $1"; (( ++failed )) }

fresh_env() {  # $1 = case dir; prints nothing, sets HOME/ZDOTDIR/PATH stubs
  local d="$TMP/$1"
  mkdir -p "$d/home" "$d/bin"
  for t in zsh python3 claude fzf; do
    printf '#!/bin/sh\nexit 0\n' > "$d/bin/$t"; chmod +x "$d/bin/$t"
  done
  # zsh stub would break the installer's own shebang usage — the installer is
  # invoked via `zsh $REPO/install.sh`, so PATH stubs only shadow checks.
  rm -f "$d/bin/zsh"
  # A no-op python3 satisfies the installer's `command -v python3` presence
  # check, but install.sh also SHELLS OUT to model-window (a real python3
  # script, via its own #!/usr/bin/env python3 shebang) to migrate legacy
  # context data — a no-op stub would silently no-op that migration too.
  # Delegate to the real interpreter so migration tests exercise real behaviour.
  printf '#!/bin/sh\nexec "%s" "$@"\n' "$(command -v python3)" > "$d/bin/python3"
  chmod +x "$d/bin/python3"
  print -r -- "$d"
}

run_installer() {  # $1 = case dir
  # </dev/null: the installer OFFERS missing optional deps
  # interactively. A test must neither block on that prompt nor let a suite
  # `brew install` anything on the machine running it.
  env -i HOME="$1/home" ZDOTDIR="$1/home" PATH="$1/bin:/usr/bin:/bin:/opt/homebrew/bin" \
    zsh "$REPO/install.sh" >"$1/out" 2>&1 </dev/null
}

# --- 1. fresh install lands every runtime component and wires the rc --------
d=$(fresh_env fresh)
run_installer "$d"; rc=$?
[[ $rc -eq 0 ]] && ok "fresh install exits 0" || { fail "fresh install rc=$rc"; cat "$d/out" }
# The installed surface is EXACTLY the manifest — nothing extra ever rides
# along (this is what keeps any other product from shipping inside ccproteus),
# and nothing missing either, which subsumes a per-component presence loop.
(( ${#module_files} )) || fail "no ccproteus-*.zsh module found beside ccproteus.sh — the manifest below would be silently short"
want=(ccproteus.sh $module_files ccproteus-adapter gateway-shim model-window settings-compose refresh-model-registry model-catalogue.json window-sources.json local.sh.example profile.conf.example profiles)
got=("$d/home/.config/ccproteus/"*(N:t))
extra=(${got:|want}); missing=(${want:|got})
(( ${#extra} == 0 && ${#missing} == 0 )) \
  && ok "install places exactly its own manifest" \
  || fail "manifest drift — extra: ${extra[*]:-none}, missing: ${missing[*]:-none}"
bin_got=("$d/home/.local/bin/"*(N:t))
[[ "${(j:,:)bin_got}" == "ccproteus" ]] \
  && ok "bin dir holds exactly the ccproteus command" || fail "bin dir: ${bin_got[*]}"
# The opt-in reference ships; the opt-in itself does not. An installed local.sh
# would silently turn features on that the installing host never asked for.
[[ -f "$d/home/.config/ccproteus/local.sh.example" ]] \
  && ok "opt-in reference installed" || fail "local.sh.example missing"
[[ -f "$d/home/.config/ccproteus/profile.conf.example" ]] \
  && ok "profile template installed" || fail "profile.conf.example missing"
[[ ! -e "$d/home/.config/ccproteus/local.sh" ]] \
  && ok "no local.sh activated on a fresh install" || fail "install activated local.sh"
# The rc must be left ALONE. ccproteus is one executable, so there is nothing to
# wire; an installer that edits a startup file makes every session's behaviour
# depend on whether one file was sourced.
grep -qF '# >>> ccproteus >>>' "$d/home/.zshrc" \
  && fail "installer wired the rc — ccproteus needs no shell integration" \
  || ok "rc left untouched"

# --- 2. legacy startup wiring is reported but never edited --------------------
print 'export BEFORE=1' > "$d/home/.zshrc"
print '# >>> ccproteus >>>' >> "$d/home/.zshrc"
print 'source "/old/path/ccproteus.sh"' >> "$d/home/.zshrc"
print '# missing end marker' >> "$d/home/.zshrc"
print 'export AFTER=1' >> "$d/home/.zshrc"
original=$(<"$d/home/.zshrc")
run_installer "$d"; rc=$?
(( rc == 0 )) && ok "legacy startup block does not fail install" || fail "legacy block install rc=$rc"
[[ "$(<"$d/home/.zshrc")" == "$original" ]] && ok "installer leaves partial rc byte-identical" || fail "installer edited partial rc"
grep -qiF 'legacy ccproteus block' "$d/out" && ok "legacy rc ownership is reported" || fail "legacy rc was not reported"

# --- 2b. an IN-PLACE install (clone already at the target) succeeds -----------
# The normal update path on a machine whose repo lives at ~/.config/ccproteus:
# SRC == CONFIG_DIR, so every copy is a copy onto itself. cp refuses that, and
# under `set -e` the run aborted before the rc was ever wired — an update that
# reported failure and left the install half-applied. Exercised by installing
# from a COPY of the repo placed at the target path.
d=$(fresh_env inplace)
target="$d/home/.config/ccproteus"
mkdir -p "$target"
for f in ccproteus.sh $module_files ccproteus-adapter gateway-shim model-window settings-compose refresh-model-registry model-catalogue.json window-sources.json local.sh.example profile.conf.example install.sh; do
  cp "$REPO/$f" "$target/$f"
done
cp "$REPO/ccproteus" "$target/ccproteus"
env -i HOME="$d/home" ZDOTDIR="$d/home" \
  PATH="$d/bin:/usr/bin:/bin:/opt/homebrew/bin" \
  zsh "$target/install.sh" >"$d/out2" 2>&1
rc=$?
[[ $rc -eq 0 ]] && ok "in-place install exits 0" \
  || { fail "in-place install rc=$rc"; cat "$d/out2" }
grep -qF '# >>> ccproteus >>>' "$d/home/.zshrc" \
  && fail "in-place install wired the rc" || ok "in-place install leaves the rc alone"
[[ -x "$d/home/.local/bin/ccproteus" ]] \
  && ok "in-place install still places the command" || fail "in-place install skipped the command"

# --- 2c. upgrading migrates legacy registry data INTO the owning profile's conf --
# An upgrade must never hand the user a chore. Installing over a pre-consolidation
# layout has to carry the probed/scoped registry data across on its own, before the
# first command runs — install.sh calls `model-window --migrate`, which attributes
# each `<profile>:<model>=<value>` row to that profile's OWN conf (as
# context_discovered) and archives the legacy file with a .migrated suffix. There
# is no cross-profile fallback and no rename-in-place: a bare (unscoped) row with no
# owning profile is left unattributed rather than guessed at.
d=$(fresh_env upgrade)
mkdir -p "$d/home/.config/ccproteus"
print -r -- 'type=apikey
base_url=http://gw.invalid
api_key=sk_test' > "$d/home/.config/ccproteus/work.conf"
print -r -- 'work:claude-opus-5=1000000' > "$d/home/.config/ccproteus/model-registry.conf"
run_installer "$d"; rc=$?
(( rc == 0 )) || { fail "upgrade install rc=$rc"; cat "$d/out" }
[[ ! -e "$d/home/.config/ccproteus/model-registry.conf" ]] \
  && ok "migration archives the legacy registry rather than leaving it live" \
  || fail "the legacy registry survived under its live name, and will be parsed as a profile"
[[ -f "$d/home/.config/ccproteus/model-registry.conf.migrated" ]] \
  && ok "the legacy registry is kept as a .migrated backup" \
  || fail "migration destroyed the legacy registry instead of archiving it"
grep -qF 'context_discovered=' "$d/home/.config/ccproteus/work.conf" \
  && ok "migration writes the probed window into the owning profile's own conf" \
  || fail "migration did not carry the scoped row into work.conf"
grep -qF '1000000' "$d/home/.config/ccproteus/work.conf" \
  && ok "migration preserves the probed value" \
  || fail "migration lost the probed window's value"

# Re-running install again (nothing left to migrate) must not error or re-migrate.
run_installer "$d"; rc=$?
(( rc == 0 )) || { fail "second install after migration rc=$rc"; cat "$d/out" }
[[ ! -e "$d/home/.config/ccproteus/model-registry" ]] \
  && ok "a second install does not resurrect the legacy registry" \
  || fail "a second install re-created the legacy registry"

# --- 2d. a module added beside ccproteus.sh ships with no installer edit -------
# The whole point of deriving the module set: parts 3–4 of the module split add
# more ccproteus-*.zsh files, and a name missed in the installer's manifest means
# a module that never ships — the library then fails to source at launch, in the
# operator's profile home, where no test can see it. The probe is inert: the
# loader names its modules explicitly, so nothing sources this one.
d=$(fresh_env probe)
mkdir -p "$d/src"
cp "$REPO"/*(.) "$d/src/"
print -r -- '# a module that exists only in this test' > "$d/src/ccproteus-zz-probe.zsh"
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin:/opt/homebrew/bin" \
  zsh "$d/src/install.sh" >"$d/out" 2>&1 </dev/null
rc=$?
(( rc == 0 )) && ok "install from a tree with an extra module exits 0" \
  || { fail "probe install rc=$rc"; cat "$d/out"; }
[[ -f "$d/home/.config/ccproteus/ccproteus-zz-probe.zsh" ]] \
  && ok "a module added beside ccproteus.sh ships with no installer edit" \
  || fail "an extra ccproteus-*.zsh was not installed — the module list is hand-copied again"
[[ ! -x "$d/home/.config/ccproteus/ccproteus-zz-probe.zsh" ]] \
  && ok "an installed module is data, not an executable" \
  || fail "an installed module carries the exec bit"

# --- 3. symlinked and partial startup files remain untouched ------------------
d=$(fresh_env symlink)
mkdir -p "$d/dotfiles"
print 'export KEEP=1' > "$d/dotfiles/zshrc"
print '# >>> ccproteus >>>' >> "$d/dotfiles/zshrc"
ln -s "$d/dotfiles/zshrc" "$d/home/.zshrc"
original=$(<"$d/dotfiles/zshrc")
run_installer "$d"
[[ -L "$d/home/.zshrc" && "$(<"$d/dotfiles/zshrc")" == "$original" ]] && ok "symlinked rc stays byte-identical" || fail "installer edited symlinked rc"

# --- 4. uninstall also leaves startup files untouched ------------------------
d=$(fresh_env uninstall)
run_installer "$d" >/dev/null
print '# >>> ccproteus >>>' >> "$d/home/.zshrc"
print 'export TAIL=1' >> "$d/home/.zshrc"
original=$(<"$d/home/.zshrc")
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" \
  zsh "$REPO/install.sh" --uninstall >/dev/null 2>&1
[[ "$(<"$d/home/.zshrc")" == "$original" ]] && ok "uninstall leaves rc byte-identical" || fail "uninstall edited rc"
[[ ! -e "$d/home/.local/bin/ccproteus" ]] && ok "uninstall removes the command" || fail "uninstall left the command on PATH"

# --- 6. a RE-RUN is an update: user state survives, and it says so ----------
# The guarantee people actually care about. Profiles (which now carry their own
# probed/manual context alongside credentials) and local.sh are never in
# COMPONENTS and are gitignored, so they were always safe — but "safe by
# construction" is exactly the kind of property that a later refactor breaks
# silently, and a re-run that looks destructive is one nobody runs twice.
d=$(fresh_env update)
run_installer "$d" >/dev/null
cfg="$d/home/.config/ccproteus"

# User state, as a real install would hold it. Probed/manual context now lives
# INSIDE the owning profile's own conf (context_discovered / context_window.*),
# not in a separate registry file — a re-run must leave that line alone too.
print 'type=apikey\napi_key=sk_test\nbase_url=https://example.invalid\nadapter=true\ncontext_window.claude-opus-5=1000000' > "$cfg/work.conf"
print 'type=sub'                                   > "$cfg/personal.conf"
print 'export CCPROTEUS_TEST_LOCAL=1'               > "$cfg/local.sh"
print 'active-marker'                              > "$cfg/active"

run_installer "$d"; rc=$?
[[ $rc -eq 0 ]] && ok "re-run exits 0" || { fail "re-run rc=$rc"; cat "$d/out" }

# Every piece of user state survives byte-for-byte.
for f in work.conf personal.conf local.sh active; do
  [[ -f "$cfg/$f" ]] || { fail "re-run destroyed $f"; continue }
done
grep -qF 'sk_test'  "$cfg/work.conf"     && ok "re-run preserved profile credentials" || fail "profile clobbered"
grep -qF 'context_window.claude-opus-5=1000000' "$cfg/work.conf" \
  && ok "re-run preserved the profile's own manual context" || fail "profile's context_window entry clobbered"
grep -qF 'CCPROTEUS_TEST_LOCAL' "$cfg/local.sh" && ok "re-run preserved local.sh"       || fail "local.sh clobbered"

# And it REPORTS an update rather than claiming a fresh install.
grep -qiF 'fresh install' "$d/out" && fail "re-run still claims 'fresh install'" || ok "re-run does not claim fresh install"
grep -qiE 'reinstalled in place|Updated ccproteus' "$d/out" && ok "re-run reports an update" || { fail "re-run did not report an update"; cat "$d/out" }
grep -qiF 'Preserved:' "$d/out" && ok "re-run names what it preserved" || fail "re-run did not name preserved state"

# And a re-run still leaves the rc alone.
grep -qF '# >>> ccproteus >>>' "$d/home/.zshrc" \
  && fail "re-run wired the rc" || ok "re-run leaves the rc alone"

# --- 7. missing optional deps warn, and never install unattended ------------
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
printf '#!/bin/sh\nexit 0\n' > "$d/bin/python3"; chmod +x "$d/bin/python3"
env -i HOME="$d/home" ZDOTDIR="$d/home" PATH="$d/bin:/usr/bin:/bin" \
  zsh "$REPO/install.sh" >"$d/out" 2>&1 </dev/null
rc=$?
[[ $rc -eq 0 ]] && ok "missing optional deps do not fail the install" || { fail "optional dep aborted install rc=$rc"; cat "$d/out" }
grep -qF 'cliproxyapi' "$d/out" && ok "absent translator is named" || fail "absent translator not mentioned"
grep -qF 'fzf'         "$d/out" && ok "absent fzf is named"        || fail "absent fzf not mentioned"
grep -qiF 'non-interactive' "$d/out" && ok "non-interactive offer declines loudly" || { fail "offer did not declare itself skipped"; cat "$d/out" }
grep -qF 'STUB BREW CALLED' "$d/out" && fail "declined offer still ran brew" || ok "declined offer never ran brew"

print ""
print "install tests: $passed passed, $failed failed"
(( failed == 0 ))
