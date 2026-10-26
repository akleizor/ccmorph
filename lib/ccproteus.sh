# ccproteus — Claude Code profile switcher
# Loaded by the `ccproteus` executable itself; not meant to be sourced
# directly into an interactive shell.

_CCPROTEUS_VERSION="2.5.1"
# The pre-2.4.0 name for `probe`, still accepted so a script that already says it keeps
# working. Assembled rather than written so the retired word appears nowhere in this
# source; a test pins that the alias still dispatches.
_ccp_retired_verb="mea""sure"

# TWO directories, deliberately. They were one, which worked only because a
# git clone was also the install: the same path held ccproteus.sh and the helper
# executables (code, replaced wholesale by an upgrade) AND every profile, the
# discovered context values, and the active marker (state, which an upgrade must never
# touch). An install breaks that coincidence — install.sh replaces the code
# wholesale on every update — so code and state must name themselves separately.
#
# STATE. Honours XDG_CONFIG_HOME, which the hardcoded path silently ignored.
_CCPROTEUS_DIR="${CCPROTEUS_STATE_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/ccproteus}"
#
# CODE: wherever this file is, its helpers sit beside it — in a clone or
# ~/.config. Derived rather than declared, so a copy of the runtime
# runs from where it was put instead of reaching back to a path that may hold a
# different version. %x is the file being sourced; :A resolves symlinks, which
# matters when a package manager links the library into place.
_CCPROTEUS_LIB_DIR="${CCPROTEUS_LIB_DIR:-${${(%):-%x}:A:h}}"
# Internal executables and data files sit beside lib/ (bin/, lib/, libexec/, share/).
_CCPROTEUS_LIBEXEC_DIR="${_CCPROTEUS_LIB_DIR:h}/libexec"
_CCPROTEUS_PROFILE_DIRS="$_CCPROTEUS_DIR/profiles"
_CCPROTEUS_CONTEXT_POLICY="${CCPROTEUS_CONTEXT_POLICY:-$HOME/.agents/runtime/policies/context.json}"
# The SHARED HARNESS LAYER: what makes Claude Code yours (skills, plugins,
# commands, hooks, output styles, memory, keybindings, history) belongs to the
# MACHINE, not to an profile — every profile links these into ~/.claude so
# behaviour cannot fork per profile. Profile state (config.json, .claude.json,
# settings.json) stays per-profile. Dirs are created-and-linked even while
# empty, so content added later reaches every profile; files are linked when
# present in ~/.claude and healed at the next launch's setup pass otherwise —
# a REAL profile-local file is respected as a deliberate override, never
# replaced. One manifest, consumed by provisioning and the fast-path check.
_CCPROTEUS_SHARED_DIRS=(
  skills hooks plugins commands output-styles agents
  session-env file-history
)
_CCPROTEUS_SHARED_FILES=(settings.local.json keybindings.json CLAUDE.md .i-have-adhd-always)
_CCPROTEUS_LOG="${TMPDIR:-/tmp}/ccproteus-${UID}.log"
_CCPROTEUS_LOG_MAX=102400  # rotate at 100KB

# The schema bounds Claude Code enforces on its own `autoCompactWindow` setting
# (.int().min(1e5).max(1e6) in the settings validator). These are NOT any model's
# window -- no window is ever written in this file; every one is probed and read
# from the registry. They exist only to keep a probed value inside the range the
# setting will accept, because a value outside it is DROPPED silently and the
# session then falls back to Claude Code's own default budget.
_CCPROTEUS_ACW_MIN=100000
_CCPROTEUS_ACW_MAX=1000000

# EPOCHREALTIME backs relay-id uniqueness; the module is NOT loaded by default
# and an unset expansion silently degrades the id to <pid>..<RANDOM>. Load it
# here (-i: never an error if unavailable) rather than depending on .zshrc.
zmodload -i zsh/datetime 2>/dev/null

# Per-shell memo for `/v1/models` listings, keyed by endpoint+credential. Declared
# here so it exists before any function reads it; see _ccp_models_listing.
#
# RESET on every source, not merely declared. `typeset -gA` alone keeps whatever a
# previous source left behind, and the memo would then outlive the endpoint state it
# describes — re-sourcing is how the suite starts a fresh scenario against the same
# base URL, and a surviving entry made eight projection tests read the PREVIOUS
# test's catalogue instead of their own. A real shell re-sources on every new
# terminal, where the same reasoning applies: a listing memoised in a shell started
# yesterday should not decide today's launch.
typeset -gA _ccp_models_memo=()

# Machine-local overrides — a gitignored file that never ships. This is the
# single seam where a host's own tooling opts into optional behavior without the
# product carrying any knowledge of that tooling. CCPROTEUS_PRELAUNCH may name an
# executable to run before launch.
# `doctor` is intentionally safe even when a host local.sh is executable zsh.
# It must not run host opt-ins or let them mutate global state.
[[ "${CCPROTEUS_DOCTOR_READONLY:-0}" != 1 && -r "$_CCPROTEUS_DIR/local.sh" ]] && source "$_CCPROTEUS_DIR/local.sh"

# ── Color (auto-off when piped) ──────────────────────────────────────────────

source "$_CCPROTEUS_LIB_DIR/ccproteus-ui.zsh" || return 1

# ── Modules ──────────────────────────────────────────────────────────────────
# Load order is the contract: the paths and globals above, then local.sh, then
# the UI module, then these modules in this order. A module only defines
# functions unless its header says otherwise; one that fails to load fails the
# whole source, as the UI module always has. Map: ARCHITECTURE.md, Code layout.
source "$_CCPROTEUS_LIB_DIR/ccproteus-base.zsh" || return 1
source "$_CCPROTEUS_LIB_DIR/ccproteus-models.zsh" || return 1
source "$_CCPROTEUS_LIB_DIR/ccproteus-context.zsh" || return 1
source "$_CCPROTEUS_LIB_DIR/ccproteus-store.zsh" || return 1
source "$_CCPROTEUS_LIB_DIR/ccproteus-profiles.zsh" || return 1
source "$_CCPROTEUS_LIB_DIR/ccproteus-cli.zsh" || return 1
source "$_CCPROTEUS_LIB_DIR/ccproteus-providers.zsh" || return 1
source "$_CCPROTEUS_LIB_DIR/ccproteus-launch.zsh" || return 1
