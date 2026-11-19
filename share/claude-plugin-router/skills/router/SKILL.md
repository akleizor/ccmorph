---
name: router
description: Delegate subagent work through ccmorph's router, which picks the profile, model and effort from the task's kind and tier and each profile's quota.
---

# ccmorph router

You decide whether to delegate. The router decides where it runs.

Call the `delegate` tool of the ccmorph-router server for delegated work; its answer is the worker's final answer. Send independent briefs as parallel calls in one message. Use the Agent tool only when the work needs this session (it runs in the background, or shares this worktree); leave its model and effort unset. When the route is another profile, the hook denies the Agent call and asks for `delegate` with the same prompt. An explicit Agent model is never overridden.

Set `kind` and `tier` yourself: `small` for search, read, list, summarise or a mechanical edit; `mid` for ordinary coding or investigation; `top` for research, design or hard multi-step debugging. Leave `profile` and `model` unset unless the user pinned them. Pass `cwd` when the task's directory differs from the session's starting directory.

A refused account waits until its window resets; a failed model waits 15 minutes. Either way the delegate retries once at the same tier, and a pinned profile or model is retried as named. Check the returned work cheaply before you rely on it.
