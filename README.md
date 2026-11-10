<div align="center">

# ccmorph

### Claude Code, on every account and every model you have.

**Your Claude subscription, Codex, Gemini, Grok, DeepSeek, Kimi: one profile each,<br>
side by side in separate terminals. Out of quota mid-task? Move the conversation<br>
to another account without losing a word.**

[![Release](https://img.shields.io/github/v/release/akleizor/ccmorph?color=111111)](https://github.com/akleizor/ccmorph/releases/latest) [![CI](https://img.shields.io/github/actions/workflow/status/akleizor/ccmorph/ci.yml?branch=main&label=CI&color=111111)](https://github.com/akleizor/ccmorph/actions/workflows/ci.yml) [![License](https://img.shields.io/badge/license-MIT-111111)](LICENSE) ![macOS](https://img.shields.io/badge/macOS-000000?logo=apple&logoColor=white) ![Linux](https://img.shields.io/badge/Linux-FCC624?logo=linux&logoColor=black)

[Install](#install) · [Why ccmorph](#why-ccmorph) · [Features](#features) · [Docs](docs/guide.md)

</div>

<br>

## Why ccmorph

You use Claude Code all day, and you have more than one way to pay for it: a
Claude subscription, a ChatGPT plan that includes Codex, a Google or xAI
sign-in, a cheap DeepSeek or Kimi key for the long refactor. Claude Code knows
about exactly one of them at a time. Switching means editing env vars, logging
out and back in, and losing the conversation you were in.

ccmorph gives each of them a **profile**, and each profile its own Claude Code:

```sh
ccmorph run claude      # your Claude subscription, terminal 1
ccmorph run codex       # your ChatGPT sign-in, terminal 2, at the same time
ccmorph run deepseek    # a DeepSeek key, terminal 3
```

Each profile has its own sign-in and settings. Your skills, plugins, hooks,
`CLAUDE.md` and session history are shared, so every profile is the same Claude
Code with a different account behind it.

## Features

<table>
<tr>
<td width="50%" valign="top">

### Hop mid-conversation

Type `/ccmorph:hop` in the Claude Code prompt, then a profile name, and the
conversation you are in moves there with its model and effort. No exit, no
copy-paste, no lost context. The status line always names the profile a
session runs on.

</td>
<td width="50%" valign="top">

### Any provider, natively

Sign in with your browser to Codex, Gemini (through Google Antigravity), Grok
(through xAI) and more, or pick DeepSeek, Kimi, GLM, Qwen, MiniMax or
OpenRouter in `ccmorph add` and paste a key: the endpoint and models are filled
in. Backends that do not speak Anthropic's API are translated for you.

</td>
</tr>
<tr>
<td valign="top">

### Gateway fixes built in

A per-session shim repairs what breaks Claude Code on third-party models: tool
ids, thinking blocks, model names and the real context window, so compaction
happens at the model's limit, not at a guessed 200K.

</td>
<td valign="top">

### Secrets stay home

Turn on `CCMORPH_MASK=1` for an API-key profile, and API keys, tokens,
passwords, card numbers and emails in a prompt become placeholders before the
request leaves your machine, then come back in the reply. The agent still
works; the provider never sees them.

</td>
</tr>
<tr>
<td valign="top">

### Every quota and bill, one screen

`ccmorph quota` shows each Claude and Codex plan's 5-hour and weekly use, each
API profile's spend today and this month, and the balance left on your
DeepSeek, Kimi or OpenRouter account. Pick the profile to hop to at a glance.

</td>
<td valign="top">

### Leaves Claude Code alone

No fork, no patch: `claude` runs unchanged, and every native flag passes
straight through. On macOS, API keys live in the login keychain.
Remove ccmorph and Claude Code is exactly as it was.

</td>
</tr>
</table>

```
$ ccmorph quota
Claude accounts and tokens
claude  Max 20x
  5h       ██░░░░░░░░░░░░░░  12%  in 4h 10m
  7d       ██████░░░░░░░░░░  40%  Tue 09:00

Codex subscriptions
codex  Plus
  5h       ██████░░░░░░░░░░  35%  in 2h 14m

API key and provider profiles
deepseek
  today $0.20 · month $3.10
  balance ¥40.75
```

## How it compares

| | ccmorph | Desktop switchers (cc-switch, Magpie) | Local routers (claude-code-router) |
|---|---|---|---|
| Profiles running side by side | **Yes, one per terminal** | One active setup per machine | One gateway, routed per request |
| Move a live conversation | **Yes, `/ccmorph:hop`** | No | No |
| Separate sign-in per profile | **Yes** | No | No |
| Rewrites Claude Code's own config | **No** | Yes | Yes |
| GUI, many agents | No: Claude Code only, in the terminal | Yes | Yes |

ccmorph does one thing: Claude Code, on any backend, done properly. If you want
one app for a dozen different agents, those tools are good at that.

## Install

```sh
brew install akleizor/tap/ccmorph
ccmorph doctor
```

Type the full `akleizor/tap/ccmorph` name: Homebrew trusts a third-party tap's
formula only when it is named that way. Without Homebrew:

```sh
sh -c "$(curl -fsSL https://github.com/akleizor/ccmorph/releases/latest/download/install.sh)"
```

You need `zsh` and the `claude` CLI, on macOS or Linux (on Windows, use WSL).
`provider` profiles, the browser sign-ins to Antigravity, Codex, Devin, Kimi For
Coding (global), Kimi For Coding (China), Meta and xAI, also need `cliproxyapi`;
the installer offers it and installs only on confirm. The
[guide](docs/guide.md#install-details) covers every install path, updating and
removal.

## Quick start

```sh
ccmorph add claude      # sign in to a Claude subscription in the browser
ccmorph add deepseek    # pick DeepSeek, paste the key
ccmorph run claude      # start Claude Code on it
```

Inside the session, type this in the prompt to move the conversation across:

```
/ccmorph:hop deepseek
```

## Usage

```sh
ccmorph run <profile>              # launch on a profile for this session
ccmorph use <profile>              # make a profile the default for `ccmorph run`
ccmorph run -i                     # choose the profile, then the model
ccmorph run <profile> --model <id> # profile plus explicit model
ccmorph run claude mcp list        # native Claude Code arguments pass through
ccmorph config <profile>           # change model, effort, or connection
ccmorph inspect [<profile>]        # effective settings and where each came from
ccmorph doctor [<profile>]         # check why a profile is not working
ccmorph quota [<profile>] [--json] # every profile's plan usage, spend and API balance
```

`/ccmorph:hop` with no profile names the others to choose from, and
`/ccmorph:profile` prints the profile the session runs on. `ccmorph --help`
shows everyday commands, and `ccmorph help <command>` the details of one.

Every turn leaves a receipt with its tokens and cost under
`~/.local/state/ccmorph/usage/`. Turn the receipts into a
[ccusage](https://github.com/ccusage/ccusage) report, one project per profile:

```sh
~/.config/ccmorph/share/usage-export --output ~/.cache/ccmorph-usage
```

## How it works

```
                    ┌─ claude    Claude subscription ───────────────────▶ Anthropic
ccmorph run <name> ─┼─ codex     browser sign-in ─▶ translator ─────────▶ Codex, Gemini, Grok
                    └─ deepseek  API key ─▶ shim (fixes, masking) ──────▶ DeepSeek, Kimi, any gateway
```

Each profile is one small file in `~/.config/ccmorph/` and its own
`CLAUDE_CONFIG_DIR`. A subscription talks to Anthropic directly. A browser
sign-in runs through `cliproxyapi`, which translates for that provider. An
API-key profile goes through a local shim started for the session, which applies
the gateway fixes and masking.

## Documentation

| | |
|---|---|
| [User guide](docs/guide.md) | Every command, setting, gateway and context-window detail |
| [Architecture](docs/ARCHITECTURE.md) | How a session reaches its backend, and why |
| [Security](.github/SECURITY.md) | Where credentials live and what protects them |
| [Remote Control add-on](addons/remote-control/README.md) | Run ccmorph sessions as Remote Control hosts |
| [Changelog](CHANGELOG.md) | What changed in each version |

## Contributing

`make test` runs the suites CI runs on Linux and macOS. See
[CONTRIBUTING](.github/CONTRIBUTING.md).

## License

MIT. See [LICENSE](LICENSE).
