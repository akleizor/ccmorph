<div align="center">

# ccmorph

### Claude Code, on every account and every model you have.

**Your Claude subscription, your company gateway, DeepSeek, Kimi, GLM: one command each,<br>
side by side in separate terminals. Out of quota mid-task? Move the conversation<br>
to another one without losing a word.**

[![Release](https://img.shields.io/github/v/release/akleizor/ccmorph?color=111111)](https://github.com/akleizor/ccmorph/releases/latest) [![CI](https://img.shields.io/github/actions/workflow/status/akleizor/ccmorph/ci.yml?branch=main&label=CI&color=111111)](https://github.com/akleizor/ccmorph/actions/workflows/ci.yml) [![License](https://img.shields.io/badge/license-MIT-111111)](LICENSE) ![macOS](https://img.shields.io/badge/macOS-000000?logo=apple&logoColor=white) ![Linux](https://img.shields.io/badge/Linux-FCC624?logo=linux&logoColor=black)

[Install](#install) · [Why ccmorph](#why-ccmorph) · [Features](#features) · [Docs](docs/guide.md)

<br>

<img src="assets/demo.gif" alt="A Claude Code session on a Claude subscription hits its 5-hour limit; /ccmorph:hop deepseek moves the same conversation to DeepSeek, which picks up the next task" width="860">

<sub>The subscription hits its limit. One command, and DeepSeek finishes the job in the same conversation.</sub>

</div>

<br>

## Why ccmorph

You use Claude Code all day, and you have more than one way to pay for it: a
Claude subscription, a company gateway, a cheap DeepSeek or Kimi key for the
long refactor. Claude Code knows about exactly one of them at a time. Switching
means editing env vars, logging out and back in, and losing the conversation
you were in.

ccmorph gives each of them a **profile**, and each profile its own Claude Code:

```sh
ccmorph run work        # your Claude subscription, terminal 1
ccmorph run company     # the company gateway, terminal 2, at the same time
ccmorph run deepseek    # DeepSeek, terminal 3
```

Your skills, plugins, hooks, `CLAUDE.md` and memory are shared by all of them.
Only the login, the settings and the history are separate, so work and personal
never mix.

## Features

<table>
<tr>
<td width="50%" valign="top">

### Hop mid-conversation

`/ccmorph:hop deepseek` inside Claude Code moves the conversation you are in to
another profile, with its model and reasoning effort. No exit, no copy-paste,
no lost context.

</td>
<td width="50%" valign="top">

### Any model, natively

DeepSeek, Kimi, GLM, MiniMax, Qwen, MiMo, StepFun, OpenRouter and SiliconFlow
are one pick in `ccmorph add`: paste a key, and the endpoint and tier models are
filled in. Endpoints that do not speak Anthropic's API are translated for you.

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

Turn on `CCMORPH_MASK=1` for a profile, and API keys, tokens and passwords in a
prompt are replaced with placeholders before the request leaves your machine,
then put back in the reply. The agent still works; the provider never sees them.

</td>
</tr>
<tr>
<td valign="top">

### Every quota and bill, one screen

`ccmorph quota` shows each subscription's 5-hour and weekly use, each
gateway's spend today and this month, and the balance left on your DeepSeek,
Kimi or OpenRouter account. Pick the profile to hop to at a glance.

</td>
<td valign="top">

### Leaves Claude Code alone

No fork, no patch: `claude` runs unchanged, and every native flag passes
straight through. Remove ccmorph and Claude Code is exactly as it was.

</td>
</tr>
</table>

```
$ ccmorph quota
Claude subscriptions
work  Max 20x
  5h       ██░░░░░░░░░░░░░░  12%  in 4h 10m
  7d       ██████░░░░░░░░░░  40%  Tue 09:00

API and gateway profiles
company
  today $18.20 · month $412.75
deepseek
  today $0.40 · month $6.39
  balance ¥40.75
```

## How it compares

| | ccmorph | Desktop switchers (cc-switch, Magpie) | Local routers (claude-code-router) |
|---|---|---|---|
| Profiles running side by side | **Yes, one per terminal** | One active setup per machine | One gateway, routed per request |
| Move a live conversation | **Yes, `/ccmorph:hop`** | No | No |
| Separate login and history per profile | **Yes** | No | No |
| Rewrites Claude Code's own config | **No** | Yes | Yes |
| GUI, many agents | No: Claude Code only, in the terminal | Yes | Yes |

ccmorph does one thing: Claude Code, on any backend, done properly. If you want
one app for a dozen different agents, those tools are good at that.

## Install

```sh
brew install akleizor/tap/ccmorph
```

<sub>Or without Homebrew: `sh -c "$(curl -fsSL https://github.com/akleizor/ccmorph/releases/latest/download/install.sh)"`. Needs `zsh` and the `claude` CLI, on macOS or Linux (on Windows, use WSL). The [guide](docs/guide.md#install-details) covers every install path, updating and removal.</sub>

## Quick start

```sh
ccmorph add work        # sign in to a Claude subscription in the browser
ccmorph add deepseek    # pick DeepSeek, paste the key
ccmorph run work        # start Claude Code on it
```

Inside the session, `/ccmorph:hop deepseek` moves you across. `ccmorph doctor`
checks the setup, and `ccmorph --help` lists every command.

## How it works

```
                    ┌─ work      Claude subscription ───────────────▶ Anthropic
ccmorph run <name> ─┼─ company   API key ─▶ shim (fixes, masking) ──▶ your gateway
                    └─ deepseek  API key ─▶ shim ─▶ translator if needed ─▶ DeepSeek, Kimi, …
```

Each profile is one small file in `~/.config/ccmorph/` and its own
`CLAUDE_CONFIG_DIR`. A subscription talks to Anthropic directly. An API-key
profile goes through a local shim, started for that session and gone when it
ends, which applies the gateway fixes and masking and writes a cost receipt per
turn. Nothing runs in the background.

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
