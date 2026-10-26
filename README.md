<div align="center">

# ccproteus

**Every Claude Code account, API key and gateway behind one command.**

[Install](#install) · [Quick start](#quick-start) · [Usage](#usage) · [Docs](docs/guide.md)

<img src="assets/demo.gif" alt="Terminal demo: a conversation started on the work profile continues on the personal profile after ccproteus hop" width="780">

</div>

## Highlights

- **One profile per backend.** Each subscription, API key or gateway gets its own
  login, settings and session history. Skills, plugins, hooks and `CLAUDE.md`
  stay shared.
- **Hop mid-session.** `ccproteus hop <profile>` moves a running conversation to
  another profile, keeping its model and effort.
- **Any endpoint.** Gateways that do not speak Anthropic's API are translated
  automatically through a per-launch adapter.
- **Cost per profile.** Every gateway turn leaves a receipt with the billed cost;
  [ccusage](https://github.com/ccusage/ccusage) turns them into daily reports.
- **Leaves `claude` alone.** No wrapper, no patch: native arguments pass through.
  See [Scope and ownership](ARCHITECTURE.md#scope-and-ownership).

```
                   +--> work       subscription login  -->  Anthropic
ccproteus run  ----+--> personal   API key             -->  Anthropic API
                   +--> gateway    API key             -->  any other model API
                                                            (translated for you)
```

## Requirements

- `zsh`, `python3`, and the `claude` CLI (the installer refuses without them)
- Optional: `cliproxyapi` for non-Anthropic endpoints, `fzf` for searchable
  pickers. The installer offers both and installs only on confirm.

## Install

```sh
git clone https://github.com/akleizor/ccproteus && cd ccproteus && ./install.sh
ccproteus --version
```

The installer copies the runtime into `~/.config/ccproteus` and the `ccproteus`
command into `~/.local/bin`. It never edits shell startup files; if
`~/.local/bin` is not on your `PATH`, it prints the line to add.

To update, re-run `./install.sh` from a fresh clone; profiles, credentials, and `local.sh` are preserved.
`./install.sh --uninstall` removes the command and leaves your profiles alone.

## Quick start

A fresh install has no profiles.

```sh
ccproteus add work         # create a profile: login or credential paste
ccproteus list             # see profiles; the default is marked
ccproteus use work         # make it the default for `ccproteus run`
ccproteus run              # launch Claude Code on the default profile
```

## Usage

```sh
ccproteus run <profile>              # launch on a profile for this session
ccproteus run -i                     # choose the profile, then the model
ccproteus run <profile> --model <id> # profile plus explicit model
ccproteus run work mcp list          # native Claude Code arguments pass through
ccproteus config <profile>           # change model, reasoning, or connection
ccproteus set <profile> model -i     # save a default model, chosen from the endpoint
ccproteus inspect [<profile>]        # effective settings and where each came from
ccproteus doctor [<profile>]         # check why a profile is not working
```

To switch a running session, use a leading `!` in the Claude Code prompt:

```
! ccproteus hop personal
```

`ccproteus --help` shows everyday commands, `ccproteus help all` the full list, and
`ccproteus help <command>` the details of one.

## Usage and cost

Each turn a gateway profile serves writes one receipt under
`~/.local/state/ccproteus/usage/`: tokens, the model that answered, and the
cost the gateway billed when it reports one. No prompt, header or credential is
stored. Export the receipts and read them with ccusage:

```sh
scripts/usage-export --output ~/.cache/ccproteus-usage
CLAUDE_CONFIG_DIR=~/.cache/ccproteus-usage ccusage claude daily --instances \
  --config ~/.cache/ccproteus-usage/ccusage.json
```

Each profile is one ccusage project. A billed cost is used as is; a turn the
gateway did not price is priced by ccusage at the provider's list price.
Subscription profiles talk to Anthropic directly and write no receipts.

## Configuration

A profile is one file, `~/.config/ccproteus/<name>.conf`, of `key=value` lines;
`profile.conf.example` is the template and lists every allowed key. Runtime state
lives under `~/.config/ccproteus/profiles/<name>`. The state root is
`$CCPROTEUS_STATE_DIR`, else `$XDG_CONFIG_HOME/ccproteus`, else `~/.config/ccproteus`.
Use `ccproteus rm` and `ccproteus mv` rather than editing files by hand.

Common keys: `type`, `base_url`, `api_key`, `model`, `effort`,
`excluded_models`, and `context_window.<model>`. To reach a private endpoint,
export `HTTPS_PROXY` for the command; ccproteus passes it through unchanged.

The [user guide](docs/guide.md) covers every setting, gateway behavior, context
windows and probing, switching, and state migration.

## Documentation

| Doc | Covers |
|---|---|
| [docs/guide.md](docs/guide.md) | Full usage and configuration reference |
| [ARCHITECTURE.md](ARCHITECTURE.md) | How sessions reach a backend, and why |
| [SECURITY.md](SECURITY.md) | Where credentials live and what protects them |
| [CHANGELOG.md](CHANGELOG.md) | What changed in each version |
| [addons/remote-control/README.md](addons/remote-control/README.md) | Remote Control bridge add-on |

## Tests

```sh
make test
```

CI runs the same suites on Linux and macOS (`.github/workflows/ci.yml`).

## License

MIT — see [LICENSE](LICENSE).
