---
name: messh
description: Manage the messh node on this device and inspect other LAN devices (status, peers, tools, calls, services, models, schedules, wake, approvals, files). Use when the user runs a messh command, asks about devices or services on the LAN, or wants work done on another device.
---

# messh

One messh node runs on every device (desktop, laptop, Pi). Agents reach
OTHER devices through the node's MCP tools; the messh CLI manages the node
on THIS device (its agents, services, approvals, schedules, network).

From inside an agent session, prefer the MCP tools for cross-device work:
`mesh_nodes`, `mesh_tools`, `mesh_call`, `mesh_copy`, `mesh_wake`,
`mesh_schedule`. They are the same operations with less output. Use the CLI
below for what MCP does not cover, or from a shell.

## Rules

- Always pass `--json` and parse the output. The human text is for people.
- Never guess a command or flag: learn them from `messh describe --json`
  (an OpenCLI 0.1 document of every command) or `messh COMMAND -h`.
- Exit codes: `0` ok. `1` failed (stderr may name the next command to run).
  `2` bad usage: fix the command. `3` no node is running for this state
  directory: tell the user to start one with `messh node`; do not start it
  yourself. `4` a person must act (approval click, code comparison, UAC
  prompt): relay the hint to the user and stop. Never try to do it yourself.
- Nothing ever prompts or waits for typed input. A command that needs a
  secret reads it from a pipe on stdin, never from typing.
- Files move by ref: `ws/...` and `artifacts/...` name files on a device.
  Copy them with `mesh_copy`, never paste their contents into calls.

## Never run these unless the user asked for that exact action

They change who is trusted, destroy state, print secrets, or answer FOR the
device owner:

- `approvals allow|always|deny`, `rules rm`, `approvals unregister`
- `pair approve|confirm|accept`, `unpair`
- `agent rm`, `agent token` (prints a secret into this transcript)
- `firewall allow|remove`, `browser reset-profile`, `service rm`

Never approve your own pending requests, and never route around an approval
(a different agent name, device, or call path) to avoid one.

## Recipes (all with --json)

Is the node up?

`messh status --json` — name, version, addresses, up since. Exit 3 means no
node: tell the user to run `messh node`.

Which devices are paired?

`messh peers --json` — name, id, online/asleep/offline, last_seen, tools.
Wake a sleeping one with `mesh_wake`.

Find a tool on a device

`mesh_tools` from MCP, or `messh tools --json` for the full local view.

Call a tool

`messh call desktop__node_info --json` — tool name, then the arguments as one
JSON object: `messh call desktop__job_status '{"job_id": "..."}' --json`.

What is waiting for approval?

`messh approvals --json` — report the requests to the user. Do not answer
them; answering is the owner's job.

Which services are registered here?

`messh service ls --json` — name, kind, URL, enabled, up/down, latency, info.

Which models are reachable?

`messh llm ls --json` — model services on the mesh and their local base URLs.

Which schedules exist?

`messh schedule ls --json` — next run and last result of each schedule.

Network trouble?

`messh doctor --json` — checks node, interfaces, firewall, multicast, peers,
each ok/warn/fail/unknown with a Fix list. Changes nothing.

## Durable jobs

Use native `job_submit` for background work on a node; remotely the tool is
`<device>__job_submit`. Prefer `command` plus `args` (not `shell`). Every
submission needs owner approval. Supply a caller-persisted `request_id` when a
submit may be retried after a lost response; retry only the same key and exact
payload. Follow the returned `job_id` with `job_status` / `job_wait`, inspect
output with `job_logs`, and use `job_list` to find jobs. Cancel only when
requested (`job_cancel`); delete only finished jobs after retrieving needed
outputs (`job_delete`). Do not treat
`pending_delivery` as target acceptance, `cancellation_pending` as confirmed
cancellation, or `stale` status as live; ask again when the target is reachable.
Do not blindly replay ambiguous non-job calls.

Jobs in an approved queue resume after node restart; approval not durably
committed must be obtained again. A running ordinary process is stopped, not
replayed. Only use `recovery` when the application implements an atomic
workspace checkpoint and resume argv; it resumes from the last committed
checkpoint, never arbitrary process memory or exactly-once side effects. See
[Native durable jobs](https://github.com/dominic-codespoti/messh/blob/main/docs/jobs.md) for the delivery, restart, and
checkpoint contract.