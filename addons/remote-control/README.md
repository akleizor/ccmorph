# Remote Control bridge (add-on)

Drive Claude Code from the Claude iPhone app on a claude.ai subscription, while every
model call is served by a ccmorph endpoint profile. You need it if you use Remote
Control in the Claude app and want those model calls on your own endpoint — a gateway
or an API key — instead of the subscription: the phone signs in with the claude.ai
account, and every model call is served by the route profile.

An add-on, not core: nothing in ccmorph knows it exists, and deleting this directory
removes the feature (see [Boundary](#boundary)).

## Requirements

- macOS: the supervisor is a launchd LaunchAgent (`com.ccmorph.rc-hosts`).
- The `claude` CLI and `zsh`, as ccmorph itself.
- Python 3.8 or newer for the launcher and the supervisor. The proxy is ccmorph's own
  (`ccmorph-core edge-proxy`); there is nothing else to install.
- Two ccmorph profiles: a Claude account profile (the account the phone
  signs in with) and an endpoint profile (the route that serves inference).
  `ccmorph add` creates both — see [First use](#first-use).

## Install

The add-on runs from a ccmorph clone, and only from one: neither of the other install
routes puts it on disk — Homebrew installs no `addons` folder, and the one-line
installer unpacks its download into a temporary directory it deletes afterwards. From
either of those, clone first. This is the clone route of the main README's
[Install](../../README.md#install) section, plus the add-on's own installer:

```sh
git clone https://github.com/akleizor/ccmorph && cd ccmorph
make shim                          # build libexec/ccmorph-core (needs Go)
./install.sh                       # ccmorph itself, from this clone
addons/remote-control/install.sh   # the add-on: commands on PATH + supervisor LaunchAgent
```

`addons/remote-control/install.sh --uninstall` stops the supervisor and removes what
the installer added. The commands are symlinked into `~/.local/bin` and run from this
tree, so the clone must stay; `git pull` and `make shim` update the add-on with it.

## First use

Create the two profiles it needs:

```sh
ccmorph add <account-profile>   # choose "Claude account": sign in with your browser
ccmorph add <route-profile>     # choose "API key": the endpoint that serves every model call
```

Then, in the folder to serve — the route argument is that endpoint profile,
`<route-profile>` in every example below:

```sh
ccmorph-rc <route-profile>                 # serve the current folder to the Claude app
ccmorph-rc <route-profile> --probe         # one prompt through the same wiring
ccmorph-rc <route-profile> -- --continue   # reattach the session last served here
```

Pass-through: anything after `--` goes to `claude remote-control` unchanged.
`ccmorph-rc-hosts` (LaunchAgent `com.ccmorph.rc-hosts`, installed by install.sh) keeps every folder listed in
`rc-hosts` served and relaunches a dead host within seconds; an archived pinned
conversation's host is deleted after `idle_days` (default 1). A host that is alive but can no longer log its
sessions in -- Claude Code read the account token when it started and the keychain
rotated it away, so every session answers with a login error while the account
profile is logged in -- is restarted at the next recovery check (inside a few
minutes), in the same folder, so its row and its sessions' ids stay; it is restarted
at most once per ten minutes, and each event is logged as `<folder>: sessions lost
their login`. A logged-out account is not restarted: signing in fixes it. A phone session whose
turn a host restart cut off is handed back to its host once that host is up again,
so it carries on without being opened on the phone. Any active phone
session left without a process (unarchived, exited while idle, crashed) is handed
back to its folder's host within ~20s, so a message never waits on a dead session; one the
server answers three times without ever starting it is parked until its next message (or
until it is opened) instead of being handed back every ten minutes, and the supervisor
keeps that count in memory, so a restart starts it over. An idle session with no
event for `park_hours` (default 6, `park_hours=0` keeps every process) is parked: its process ends to free
memory, and its next message wakes it within ~20s. An archived session's kept worktree is removed
after `idle_days` (unarchiving still resumes it), and the session itself is deleted
after `purge_days` (default 7); either step skips a worktree holding uncommitted or
unmerged work. A days value that is not a valid number falls back to its default,
never a crash.
One folder is one host: `.claude/rc-host.json` in the folder keeps its environment,
so a restart, reboot or Ctrl-C reattaches the same row in the app and its session
instead of registering a new one. `ccmorph-rc` marks the folder trusted before
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

`--as <account-profile>` picks the claude.ai account when there is more than one
`type=sub` profile. `--debug` writes one line per request (method, path, who served it) to
`profiles/.rc-bridge/debug.log`; no headers or bodies are ever logged.

## After a Mac restart

Nothing starts by itself. From any terminal, including SSH from the phone, serve
your home folder as one generic entry the app can start sessions in:

```sh
tmux new-session -d -s rc-bridge -c ~ 'ccmorph-rc <route-profile> -- --no-create-session-in-dir'
```

Sessions started from the phone begin in `~` and can move into any repository;
that repository's CLAUDE.md, settings and hooks then apply.

If Claude refuses with "This folder is already served", a server that was killed
rather than stopped left `~/.claude/projects/<folder>/bridge-pointer.json` behind.
Check the `pid` in it is gone, move the file aside, and start again.

Credentials inherited from the calling shell (`CLAUDE_CODE_OAUTH_TOKEN`,
`ANTHROPIC_API_KEY`, ...) are dropped, as in every ccmorph launch: the session
always signs in with the account profile's own claude.ai login.

## Switch inference live

```sh
ccmorph-rc <route-profile> --switch  # every host's next turn goes to that profile
ccmorph-rc <route-profile> --switch  # an account profile the phone signs in with (Anthropic, its own quota)
ccmorph-rc <route-profile> --switch  # back to the edge's route
```

No host restarts and no session is interrupted: each proxy reads the switch on every
turn. The account profile sends calls to Anthropic untouched; a profile whose own edge
is live is that edge's route (`route`); any other route or provider profile is brought
up beside the one live shared edge and handed to that edge's proxy through a 0600 file
it deletes on read, so its key stays in memory only. A key profile gets one adapter per
edge (`sidecar-<profile>.json`): a repeat `--switch` reuses it while it answers, or
stops it and confirms it gone before starting its replacement. Its model map is
applied, so a provider such as codex receives its own models. When two switches
overlap, the one started last wins: an older one that finishes after it writes
nothing, and says which profile is live.

`inference=<profile>` in `rc-hosts` does the same from the config: the supervisor
brings the profile up once exactly one shared edge is live, retries until it lands,
and writes the switch only when the `inference=` value itself changes, so a manual
`--switch` stays until that line is edited. A proxy holds a handoff in memory only,
so an edge or proxy restart loses it; the choice is to hand it again, never to keep
the key on disk. Each proxy says what it holds in `held.<edge pid>.json` (profile
names and handoff ids, no key), and the supervisor re-runs `--switch` for the live
profile whenever the live edge's proxy does not hold it. A proxy started before this
feature needs one restart to load it.

```sh
ccmorph-rc <route-profile> --switch --session cse_...  # only that phone session moves
ccmorph-rc <route-profile> --unswitch cse_...           # it follows the shared switch again
```

Every request carries its session's id (`X-Claude-Code-Session-Id`, the same for its
subagents: a UUIDv5 Claude Code derives from the session's `--sdk-url`, which the proxy
matches to the `cse_...` id), so one session can run on its own profile while the rest
share the switch.
With the router on (`ccmorph router on`), one subagent can run on another profile
too: the router's Agent hook has the edge hold the profile it picked (`--switch
--rehand`, no word changes) and leaves the pick for the proxy, which sends that one
agent's requests (`X-Claude-Code-Agent-Id`) to it. The subagent stays Claude Code's
own: it shows in the session, streams, and returns to its caller as usual.
A profile already up beside the live edge with the same tiers is reused, not started
again. A tier the route pinned outside the Claude family (fable on a gateway model)
reaches another target as that target's own model for the alias.
`ccmorph-rc-hosts --list-sessions` prints the active sessions and each one's pick as
JSON, and removes a pick whose session is gone. A conversation that moves between
providers carries thinking blocks the previous one signed; a provider may refuse them.

The proxy takes a handoff within a second, not on the next request for that profile,
and only the proxy of the edge it names takes it: `--switch` waits until that proxy
says it holds it, and removes the handoff and leaves the word as it was when it does
not. What the proxy does with each request, the key and the endpoints are
compiled into `ccmorph-core` (`agentproxy/edge*.go`), so a change to them, or to the
launcher, needs the edge restarted after `make shim`. Restarting the edge alone (its fixed port and CA stay) does not stop
a session's process; restarting `ccmorph-rc-hosts` stops every host and its sessions.

`ccmorph-rc-hosts` pins the route profile's tiers (each alias its profile maps outside
its own Claude family, as `ccmorph get <route> --json` reports them); a
`tier.<alias>=<model>` line in `rc-hosts` overrides one. A pinned tier names the route's
model on that bare alias, so under another target pick that model by its full id instead.
At every config load the supervisor publishes the routes it hosts and those overrides to
`profiles/.rc-bridge/hosted.json` (removed when it stops cleanly), stamped with its pid
and start so a record a killed supervisor left behind is never believed; `ccmorph doctor
<profile>` compares that profile's edge against it.

## How it works

Remote Control is offered only to a claude.ai-authenticated process talking to
api.anthropic.com, so that stays true. A loopback proxy (`ccmorph-core edge-proxy`), trusted by this process
tree only, moves `api.anthropic.com/v1/messages` to the route profile (through its
`ccmorph-core adapter` when the endpoint needs one). Bridge, OAuth and telemetry traffic
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
| The claude.ai credential never reaches the route | `agentproxy/edge_proxy_test.go` (`TestEdge*`) |
| A dead route is an error, never a fallback to the subscription | `agentproxy/edge_proxy_test.go` (dead route and no-first-party tests) |
| The route key is in no argv, environment, or lingering file | 0600 file deleted on read; the proxy binary is checked before the file is written, and every later exit including TERM/HUP/QUIT/INT unlinks it (a signal the caller ignores stays ignored); a `--switch` handoff holding the key is staged in a 0600 temp only that switch owns and written with TERM/HUP/QUIT/INT deferred until it is live or gone; `tests/rc_bridge_test.py` (`-k 'no_failed_start or termination_during_proxy_startup or as_the_route_key_file or termination_during_a_switch or switch_whose'`) and `tests/rc_route_key_repro_test.py` |
| Routed responses are verified, not assumed | every inference turn's reply line pairs the word the pick set with the endpoint that answered (`asked=` / `served-by=`, `via=` when a fallback routed it); Anthropic's organization header is the first-party proof and the response-side connection's host and port are the endpoint proof (the address the connection was dialled for, never the resolved peer), so a turn answered by anything else — a routed turn carrying the header, an account turn without it, a reply from an endpoint other than the one the request left for, or a pick the proxy holds no endpoint for (it falls back to the edge's route, issue #595) — is logged `miss` (#920: requested-vs-served per session turn, so a pick that is set but not served says so); `agentproxy/edge_test.go` |
| A session's own pick is kept as long as its word stands | the supervisor asks the live edge's proxy what it holds every tick and re-hands every session's profile word it does not hold (`Switcher.rehand`, #595's upkeep for the shared word widened to `--switch --session` words — GH-977: after an edge or proxy restart only the shared word came back and the session fell to the route while the phone still showed the pick); a word whose re-hand child failed is tried behind the words that have not, so one deleted profile cannot keep another session's pick unserved; the 2026-11-25 miss (Stark's pick not served) was `1aa363d` (#908: Claude Code sends a UUIDv5 of the `--sdk-url`, not the `cse_` id, so no per-session pick matched) and #920's `14c8d09` verdict reached only the `--debug` log, so this row keeps the pick across an edge or proxy restart and the next row reports a miss without `--debug`; `tests/rc_hosts_test.py` (`-k 'session_word or a_word_whose_rehand_fails'`) |
| A pick that was not served is reported without `--debug` | the edge's proxy keeps this edge run's asked-vs-served outcome — turns, misses, the last miss with its line — in `<bridge>/served.<edge pid>.json`, believed only together with the `held.<edge pid>.json` of the same start and only while that start is the running edge process's own, read without ever waiting on a FIFO under the name, gone with the proxy and with a swept-away edge; `doctor` answers it for the hosted route and warns with the last miss (GH-977: the #920 (`14c8d09`) verdict reached only the debug log, so the phone found each miss first); `agentproxy/edgeserved_test.go` (`TestTheOutcome*`, `TestAFifo*`, `TestTheFirstReply*`), `tests/doctor_test.py` (`-k gh_977`) |
| No system trust change | CA lives in `profiles/.rc-bridge/edge-ca` (0700); only `NODE_EXTRA_CA_CERTS` of this process |
| No orphans | proxy and adapter follow the Claude pid and exit with it; a start that fails stops the proxy it spawned (`tests/rc_bridge_test.py -k failed_start_stops`) |
| A network outage never restarts the edge or a running host; Claude Code's own backoff reconnects, the supervisor relaunches a bridge that quit and hands back processless sessions | `edge_down` probes loopback only; `tests/rc_bridge_test.py`, `tests/rc_hosts_test.py` (`-k outage`) |

## Boundary

Uses only `ccmorph _route_env`/`_route_stop` (the route profile, plus a running
bridge when it is a provider), `ccmorph _env` (the account), `ccmorph _backend_env` (the names a launch never
inherits) and
`ccmorph-core adapter <profile> --auto --parent-pid`.
ccmorph core names no file or path of this directory; deleting it removes the feature.
The one seam is an environment variable, not a file: the session carries
`CCMORPH_EDGE_ROUTE_ENV` (`KEY=VALUE` per line, the effective values of
`CLAUDE_CODE_DISABLE_ADVISOR_TOOL` and `CLAUDE_CODE_MODEL_CAPABILITIES`, adapter
defaults included), and a `ccmorph run` started inside the session re-applies those
lines when its `HTTPS_PROXY` is a loopback proxy, so the nested request is not
rejected by the route. Without the add-on the variable is never set and nothing changes.

## Caveats

- Relies on Claude Code sending inference to `/v1/messages` and not pinning
  certificates. A release that changes either breaks this loudly (errors), not silently.
- Using a subscription's Remote Control with another inference backend may conflict
  with Anthropic's terms. That judgement belongs to the operator.
