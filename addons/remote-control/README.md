# Remote Control bridge (add-on)

Drive Claude Code from the Claude iPhone app on a claude.ai subscription, while every
model call is served by a ccproteus endpoint profile.

```sh
addons/remote-control/install.sh          # commands on PATH + supervisor LaunchAgent
ccproteus-rc gateway                       # serve the current folder to the Claude app
ccproteus-rc gateway --probe               # one prompt through the same wiring
ccproteus-rc gateway -- --continue         # reattach the session last served here
```

Pass-through: anything after `--` goes to `claude remote-control` unchanged.
`ccproteus-rc-hosts` (LaunchAgent `com.ccproteus.rc-hosts`, installed by install.sh) keeps every folder listed in
`rc-hosts` served and relaunches a dead host within seconds; an archived pinned
conversation's host is deleted after `idle_days` (default 1). A phone session whose
turn a host restart cut off is handed back to its host once that host is up again,
so it carries on without being opened on the phone. Any active phone
session left without a process (unarchived, exited while idle, crashed) is handed
back to its folder's host within ~20s, so a message never waits on a dead session; an archived session's kept worktree is removed
after `idle_days` (unarchiving still resumes it), and the session itself is deleted
after `purge_days` (default 7); either step skips a worktree holding uncommitted or
unmerged work. A days value that is not a valid number falls back to its default,
never a crash.
One folder is one host: `.claude/rc-host.json` in the folder keeps its environment,
so a restart, reboot or Ctrl-C reattaches the same row in the app and its session
instead of registering a new one. `ccproteus-rc` marks the folder trusted before
launching, so nothing waits on a first-run prompt.

Thinking: shown collapsed as "omitted" (a gateway rejects the live-thinking mode, and
its summarized mode costs ~2.5s per Opus turn on the measured gateway).

Billing header: a claude.ai login makes Claude Code write per-request fields
(`cch`, `cc_prev_req`, `cc_prompt_id`, `cc_turn_origin`, `cc_prompt_index`,
`cc_turn_index`) into the `x-anthropic-billing-header:` block at `system[0]`, so every
turn's body differed from the last and a route caching on the request prefix never hit:
the ledger showed turns writing 100k+ tokens of prompt again while reading almost none
back. The proxy drops those six and keeps the header, which is what a gateway launch of
the same Claude Code sends.

Models: `fable`/`opus`/`sonnet`/`haiku` mean the real Claude models, 1:1, served by
the route; none is substituted. A tier pinned by the caller (`ANTHROPIC_DEFAULT_FABLE_MODEL=gpt-6-astra`)
covers the whole family: the proxy rewrites full ids (`claude-fable-5-1`) too.
`--route-models` uses the route profile's own alias map instead.

`--as <profile>` picks the claude.ai account when there is more than one `type=sub`
profile. `--debug` writes one line per request (method, path, who served it) to
`profiles/.rc-bridge/debug.log`; no headers or bodies are ever logged.

## After a Mac restart

Nothing starts by itself. From any terminal, including SSH from the phone, serve
your home folder as one generic entry the app can start sessions in:

```sh
tmux new-session -d -s rc-bridge -c ~ 'ccproteus-rc gateway -- --no-create-session-in-dir'
```

Sessions started from the phone begin in `~` and can move into any repository;
that repository's CLAUDE.md, settings and hooks then apply.

If Claude refuses with "This folder is already served", a server that was killed
rather than stopped left `~/.claude/projects/<folder>/bridge-pointer.json` behind.
Check the `pid` in it is gone, move the file aside, and start again.

Credentials inherited from the calling shell (`CLAUDE_CODE_OAUTH_TOKEN`,
`ANTHROPIC_API_KEY`, ...) are dropped, as in every ccproteus launch: the session
always signs in with the account profile's own claude.ai login.

## How it works

Remote Control is offered only to a claude.ai-authenticated process talking to
api.anthropic.com, so that stays true. A loopback mitmproxy, trusted by this process
tree only, moves `api.anthropic.com/v1/messages` to the route profile (through its
`ccproteus-adapter` when the endpoint needs one). Bridge, OAuth and telemetry traffic
reach Anthropic untouched; other hosts are not intercepted at all.

One exception, for open speed: the phone opens a session by downloading every event
the host uploaded, and it only renders that copy. The proxy lightens those uploads
(`worker/events`) by about 60%: command and agent descriptions, thinking signatures,
second copies of tool inputs and outputs, tool output past 4,000 characters, and
screenshots over ~75 KB (re-encoded as 1000 px JPEG). The model's own transcript on
the Mac is never touched.

## Guarantees and their checks

| Guarantee | Enforced by |
|---|---|
| The claude.ai credential never reaches the route | `rc_route.reroute`; `tests/rc_bridge_test.py` |
| A dead route is an error, never a fallback to the subscription | `tests/rc_bridge_failclosed_test.py` |
| The route key is in no argv, environment, or lingering file | 0600 file deleted on read; same test |
| Routed responses are verified, not assumed | `served-by=` in the debug log flags any response carrying Anthropic's organization header |
| No system trust change | CA lives in `profiles/.rc-bridge/mitm` (0700); only `NODE_EXTRA_CA_CERTS` of this process |
| No orphans | proxy and adapter follow the Claude pid and exit with it |
| A network outage never restarts the edge or a running host; Claude Code's own backoff reconnects, the supervisor relaunches a bridge that quit and hands back processless sessions | `edge_down` probes loopback only; `tests/rc_bridge_test.py`, `tests/rc_hosts_test.py` (`-k outage`) |

## Boundary

Uses only `ccproteus _route_env`/`_route_stop` (the route profile, plus a running
bridge when it is a provider), `ccproteus _env` (the account), `ccproteus _backend_env` (the names a launch never
inherits) and
`ccproteus-adapter <profile> --auto --parent-pid`.
ccproteus core names no file or path of this directory; deleting it removes the feature.
The one seam is an environment variable, not a file: the session carries
`CCPROTEUS_EDGE_ROUTE_ENV` (`KEY=VALUE` per line, the effective values of
`CLAUDE_CODE_DISABLE_ADVISOR_TOOL` and `CLAUDE_CODE_MODEL_CAPABILITIES`, adapter
defaults included), and a `ccproteus run` started inside the session re-applies those
lines when its `HTTPS_PROXY` is a loopback proxy, so the nested request is not
rejected by the route. Without the add-on the variable is never set and nothing changes.

## Caveats

- Relies on Claude Code sending inference to `/v1/messages` and not pinning
  certificates. A release that changes either breaks this loudly (errors), not silently.
- Using a subscription's Remote Control with another inference backend may conflict
  with Anthropic's terms. That judgement belongs to the operator.
