# ccmorph — Claude Code profile switcher
# Loaded by the `ccmorph` executable itself; not meant to be sourced
# directly into an interactive shell.

_CCMORPH_VERSION="2.5.1"
# The pre-2.4.0 name for `probe`, still accepted so a script that already says it keeps
# working. Assembled rather than written so the retired word appears nowhere in this
# source; a test pins that the alias still dispatches.
_ccm_retired_verb="mea""sure"

# TWO directories, deliberately. They were one, which worked only because a
# git clone was also the install: the same path held ccmorph.sh and the helper
# executables (code, replaced wholesale by an upgrade) AND every profile, the
# discovered context values, and the active marker (state, which an upgrade must never
# touch). An install breaks that coincidence — install.sh replaces the code
# wholesale on every update — so code and state must name themselves separately.
#
# STATE. Honours XDG_CONFIG_HOME, which the hardcoded path silently ignored.
_CCMORPH_DIR="${CCMORPH_STATE_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/ccmorph}"
#
# CODE: wherever this file is, its helpers sit beside it — in a clone or
# ~/.config. Derived rather than declared, so a copy of the runtime
# runs from where it was put instead of reaching back to a path that may hold a
# different version. %x is the file being sourced; :A resolves symlinks, which
# matters when a package manager links the library into place.
_CCMORPH_LIB_DIR="${CCMORPH_LIB_DIR:-${${(%):-%x}:A:h}}"
# Internal executables and data files sit beside lib/ (bin/, lib/, libexec/, share/).
_CCMORPH_LIBEXEC_DIR="${_CCMORPH_LIB_DIR:h}/libexec"
_CCMORPH_PROFILE_DIRS="$_CCMORPH_DIR/profiles"
_CCMORPH_CONTEXT_POLICY="${CCMORPH_CONTEXT_POLICY:-$HOME/.agents/runtime/policies/context.json}"
# The SHARED HARNESS LAYER: what makes Claude Code yours (skills, plugins,
# commands, hooks, output styles, memory, keybindings, history) belongs to the
# MACHINE, not to an profile — every profile links these into ~/.claude so
# behaviour cannot fork per profile. Profile state (config.json, .claude.json,
# settings.json) stays per-profile. Dirs are created-and-linked even while
# empty, so content added later reaches every profile; files are linked when
# present in ~/.claude and healed at the next launch's setup pass otherwise —
# a REAL profile-local file is respected as a deliberate override, never
# replaced. One manifest, consumed by provisioning and the fast-path check.
_CCMORPH_SHARED_DIRS=(
  skills hooks plugins commands output-styles agents
  session-env file-history
)
_CCMORPH_SHARED_FILES=(settings.local.json keybindings.json CLAUDE.md .i-have-adhd-always)
_CCMORPH_LOG="${TMPDIR:-/tmp}/ccmorph-${UID}.log"
_CCMORPH_LOG_MAX=102400  # rotate at 100KB

# The schema bounds Claude Code enforces on its own `autoCompactWindow` setting
# (.int().min(1e5).max(1e6) in the settings validator). These are NOT any model's
# window -- no window is ever written in this file; every one is probed and read
# from the registry. They exist only to keep a probed value inside the range the
# setting will accept, because a value outside it is DROPPED silently and the
# session then falls back to Claude Code's own default budget.
_CCMORPH_ACW_MIN=100000
_CCMORPH_ACW_MAX=1000000

# EPOCHREALTIME backs relay-id uniqueness; the module is NOT loaded by default
# and an unset expansion silently degrades the id to <pid>..<RANDOM>. Load it
# here (-i: never an error if unavailable) rather than depending on .zshrc.
zmodload -i zsh/datetime 2>/dev/null

# Per-shell memo for `/v1/models` listings, keyed by endpoint+credential. Declared
# here so it exists before any function reads it; see _ccm_models_listing.
#
# RESET on every source, not merely declared. `typeset -gA` alone keeps whatever a
# previous source left behind, and the memo would then outlive the endpoint state it
# describes — re-sourcing is how the suite starts a fresh scenario against the same
# base URL, and a surviving entry made eight projection tests read the PREVIOUS
# test's catalogue instead of their own. A real shell re-sources on every new
# terminal, where the same reasoning applies: a listing memoised in a shell started
# yesterday should not decide today's launch.
typeset -gA _ccm_models_memo=()

# Machine-local overrides — a gitignored file that never ships. This is the
# single seam where a host's own tooling opts into optional behavior without the
# product carrying any knowledge of that tooling. CCMORPH_PRELAUNCH may name an
# executable to run before launch.
# `doctor` is intentionally safe even when a host local.sh is executable zsh.
# It must not run host opt-ins or let them mutate global state.
[[ "${CCMORPH_DOCTOR_READONLY:-0}" != 1 && -r "$_CCMORPH_DIR/local.sh" ]] && source "$_CCMORPH_DIR/local.sh"

# ── Color (auto-off when piped) ──────────────────────────────────────────────

source "$_CCMORPH_LIB_DIR/ccmorph-ui.zsh" || return 1

# ── Modules ──────────────────────────────────────────────────────────────────
# Load order is the contract: the paths and globals above, then local.sh, then
# the UI module, then these modules in this order. A module only defines
# functions unless its header says otherwise; one that fails to load fails the
# whole source, as the UI module always has. Map: docs/ARCHITECTURE.md, Code layout.
source "$_CCMORPH_LIB_DIR/ccmorph-base.zsh" || return 1
source "$_CCMORPH_LIB_DIR/ccmorph-models.zsh" || return 1
source "$_CCMORPH_LIB_DIR/ccmorph-context.zsh" || return 1
source "$_CCMORPH_LIB_DIR/ccmorph-store.zsh" || return 1
source "$_CCMORPH_LIB_DIR/ccmorph-profiles.zsh" || return 1
source "$_CCMORPH_LIB_DIR/ccmorph-cli.zsh" || return 1
source "$_CCMORPH_LIB_DIR/ccmorph-providers.zsh" || return 1
source "$_CCMORPH_LIB_DIR/ccmorph-launch.zsh" || return 1
