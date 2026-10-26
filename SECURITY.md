# Security

ccproteus stores credentials on disk. This document says exactly where, what
protects them, and what does not — so you can decide whether to trust it with an
API key before you do.

## Reporting a vulnerability

Open an issue for anything already public. For anything that would expose a
credential if described publicly, contact the maintainer directly rather than
filing a public issue, and allow time for a fix before disclosure.

Include the ccproteus version (`ccproteus --version`), your OS and zsh version, and
the smallest reproduction you have. Never paste a real key, token, or `.conf` file
into a report — describe the shape instead.

## Supported versions

The latest released version. There are no backported security fixes for older
tags; update rather than patch in place.

## What is stored, and where

| Path | Holds | Mode |
|---|---|---|
| `~/.config/ccproteus/<name>.conf` | one profile: API key, OAuth token, or an email for a subscription login | `600` |
| `~/.config/ccproteus/` | the state root (honours `CCPROTEUS_STATE_DIR`, then `XDG_CONFIG_HOME`) | `700` |
| `~/.config/ccproteus/profiles/<name>/` | that profile's Claude Code config, including its own auth state | `700` |
| `${TMPDIR}/ccproteus-<uid>.log` | launch and adapter diagnostics | ephemeral; created `600`, and an existing log of ours is tightened to `600` |
| `<profile>/adapter/` | a rendered translator config, created per session | `700`, removed on exit |

Every `*.conf` is gitignored, including the manual `context_window.<model>`
overrides and the discovered `context_discovered` metadata it holds. No
credential has ever been tracked in this repository.

## What protects them

- **Atomic writes.** A profile is written to a `mktemp` file and renamed into
  place, so an interrupted write cannot leave a half-file that still parses.
- **Modes set before content.** Files are created `600` and directories `700`
  before anything is written to them, not fixed up afterwards.
- **Symlink refusal.** ccproteus removes a profile-local `settings.json` only when
  it is a symlink, and refuses to generate settings through a symlinked
  `settings.json` or `settings.profile.json`. Writers refuse to follow a symlink
  into a location they did not choose.
- **No credential in generated settings.** A profile's `settings.json` is generated,
  but every variable a launch sets — endpoint and API key included — is left out of
  its `env` block; credentials reach Claude Code only through the process
  environment.
- **No credential in a log or an error.** No log line or error message
  interpolates a key, a token, or a raw `auth status` payload.
- **No credential on a command line.** Credentials reach Claude Code and the
  translator through the environment or a `600` file, never as an argument, so
  they do not appear in `ps` output.
- **Secret prompts do not echo.** `ccproteus add` reads keys and tokens with
  `read -rs`, so they do not reach your terminal or your shell history.
- **Per-credential isolation.** A profile holding a credential gets its own
  translator instance. A shared instance would let one profile spend another's
  credential, because the translator's inbound key list is instance-wide.
- **A non-subscription profile does not inherit your subscription.** When seeding
  a new profile, OAuth caches and subscription markers are stripped.
- **The one-time profile-directory move renames, never copies.** Relocating
  profile state from `~/.claude-profiles` into the state root is an `os.rename`
  under an exclusive lock, so no credential is duplicated on disk and no mode is
  re-derived. It refuses to merge or overwrite when both locations hold real
  directories, refuses a cross-device move rather than falling back to a copy, and
  rolls back if it cannot leave the legacy path behind as a compatibility symlink.
  Native `~/.claude` is not touched.

## What does not protect them

Know these before trusting the tool with a key.

- **Credentials are stored unencrypted**, protected by file permissions alone.
  Anything running as your user can read them. There is no keychain integration
  and no passphrase.
- **`ccproteus cred <profile>` prints a stored credential to stdout** by design,
  for piping into another command. Redirect them deliberately; a shell that logs
  command output will capture the value.
- **`local.sh` is trusted executable zsh**, sourced into ccproteus's process on
  launch. Anyone who can write it can already write `lib/ccproteus.sh` beside it, so it
  grants no new privilege — but it is code, not configuration. Read anything you
  copy into it.
- **`--uninstall` deliberately leaves your profiles in place**, credentials and
  per-profile runtime state included. Removing the state root
  (`~/.config/ccproteus`, which now holds `profiles/`) is a separate, manual step.
- **A gateway you point a profile at receives your prompts.** ccproteus does not
  inspect, filter, or retain them, and adds no telemetry of its own — but pointing
  a profile at an endpoint is a decision to send your conversations there.
- **Discovered context limits (`context_discovered` in a profile's `.conf`) are
  machine-local and route-bound.** Never copy another host's `.conf` wholesale:
  its ceilings were probed against that host's route, and a wrong ceiling means
  a session that dies mid-conversation instead of compacting. A route change
  invalidates the stored discovery automatically; a manual `context_window.<model>`
  override is not route-bound and travels with the profile on purpose.

## Network behavior

ccproteus itself makes only metadata requests — model listings and reachability
probes — never inference, except where it says so:

- `ccproteus probe` is metadata-only by default. Context-limit inference requires
  `--allow-inference` and a selected profile/model. Accepted requests may be billed.
- Automatic compatibility uses cache and per-model protocol metadata first. If
  those cannot answer, it may send up to four requests for the selected model,
  capped at 8 output tokens for Messages/Chat or 16 for Responses within a
  30-second classification deadline. Charges are disclosed before the check;
  successful verdicts are cached. No selected model means no paid inventory sweep.

`--check` skips persistence; inference remains a separate explicit opt-in.

Command, installation, and host-integration boundaries are defined in
[Scope and ownership](ARCHITECTURE.md#scope-and-ownership).
