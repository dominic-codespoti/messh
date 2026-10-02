# messh

messh connects Windows and Linux devices on a trusted LAN so an AI agent can use
resources on another machine: a desktop GPU, a local model server, a browser
session, or files. Every device runs the same `messh node`. Agents connect to
their local node through [MCP](https://modelcontextprotocol.io/), or the bundled
pi extension; paired nodes communicate over mutually authenticated TLS.

**Experimental software.** Start with read-only calls and devices you own.
Approval prompts are a consent mechanism, not an OS sandbox. Read the
[security model](#security-model) before giving an agent access to your mesh.

## Features

- Two-sided device pairing, LAN discovery, and pinned TLS 1.3 peer connections.
- Compact agent tools for discovering and calling tools across the mesh.
- Background jobs with approval, logs, resource scheduling, and process-tree cancellation.
- Local MCP, OpenAPI, HTTP, and OpenAI-compatible services shared through one catalogue.
- A streaming model proxy for using another device's local LLM as an agent provider.
- File transfer with end-to-end SHA-256 verification between workspace/artifact directories.
- Optional browser control with per-website approval and a dedicated sign-in profile.
- Scheduled tool calls, Wake-on-LAN, desktop approvals, audit records, and network diagnostics.
- An agent-friendly CLI and a [native pi extension](integrations/pi/README.md).

## Contents

- [Requirements and supported platforms](#requirements-and-supported-platforms)
- [Quick start](#quick-start): [install](#build-and-install), [start nodes](#run-a-node-on-every-device),
  [pair devices](#pair-devices), [connect agents](#connect-agents), [read-only smoke call](#read-only-smoke-call)
- [Architecture](#architecture)
- [CLI and bundled agent skill](#using-messh-from-an-agents-shell)
- [Network setup](#network-setup)
- [Tool reference](#tools)
- [Services](#services) and [remote models](#use-a-model-on-another-device)
- [Jobs](#jobs), [browser](#browser), and [approvals](#approvals)
- [Files](#files), [scheduling](#scheduling), and [Wake-on-LAN](#wake-on-lan)
- [Security model](#security-model) and [state](#state)
- [Limitations](#limitations) and [troubleshooting](#troubleshooting)
- [Development](#development), [CI and releases](#ci-and-releases), and [license](#license)

## Requirements and supported platforms

| Platform | Supported target | Notes |
|---|---|---|
| Windows | `windows/amd64` | Run the node as your normal user in the interactive login session for desktop approvals and browser access. It is not a Windows service. |
| Linux | `linux/amd64`, `linux/arm64` | Desktop and headless nodes, including a Raspberry Pi with a 64-bit Linux OS. A desktop session is optional. |
| macOS | Not supported | There is no macOS implementation or release target. |

Building requires **Go 1.27.1 or newer**, as declared in [`go.mod`](go.mod).
The binary is pure Go: no cgo, Node.js, Python, Docker, or GPU runtime is required
to start a node, pair devices, or use the core mesh tools.

Optional dependencies are feature-specific:

- **Browser:** Node.js 18+ with `npx`, an installed Chrome/Edge/compatible Chromium
  browser, and Playwright MCP (downloaded on demand). See [Browser](#browser).
  Browser Node.js requirements are separate from the **Node.js 24** used to
  develop and test the pi extension.
- **pi:** an existing pi installation; the extension adds no runtime npm
  dependencies. See its [installation guide](integrations/pi/README.md).
- **Linux desktop approvals:** libnotify 0.8+ `notify-send`, a notification daemon
  supporting actions, and `busctl` or `gdbus`. `zenity` provides a dialog fallback.
  Without them, use the node's terminal or `messh approvals`.
- **Linux OS integration:** a usable systemd user manager for optional job memory
  limits (`systemd-run`) and a user service; `systemd-inhibit`/logind for sleep
  inhibition, with `gdbus` or `dbus-monitor` for sleep events. Core operation does
  not require systemd; unavailable integration is reported.
- **Hardware/services:** install the programs jobs will run and any model/service
  servers separately. NVIDIA live VRAM probing uses `nvidia-smi`; Wake-on-LAN
  requires support and configuration on the target's hardware, driver, and network.

## Quick start

Use two devices on the same trusted LAN. Build or install on both, start one node
per device, compare pairing codes on both sides, then register an agent on the
device where that agent runs. Keep nodes running while using the other commands
in separate terminals.

### Build and install

Clone the public repository:

```sh
git clone https://github.com/dominic-codespoti/messh.git
cd messh
```

Install the CLI from the checkout:

```sh
go install ./cmd/messh
```

Add `GOBIN` (or the `bin` directory under `go env GOPATH` when `GOBIN` is unset)
to your `PATH`. This installs `messh.exe` on Windows and `messh` on Linux.
Alternatively, build an executable and put it on `PATH` yourself:

```powershell
# Windows PowerShell
go build -o messh.exe ./cmd/messh
.\messh.exe -h
```

```sh
# Linux
go build -o messh ./cmd/messh
./messh -h

# Cross-compile from a POSIX shell for a 64-bit Raspberry Pi
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o messh-linux-arm64 ./cmd/messh
```

For downloadable builds, see [CI and releases](#ci-and-releases). Successful
trusted `main` pushes publish immutable main-channel prereleases; SemVer tags
publish tagged releases. Source installation does not require an existing release.

### Run a node on every device

```sh
messh node --name desktop    # on device A
messh node --name worker     # on device B, in its own terminal
```

The node listens on TCP 7519 for peers (connections from outside loopback,
private, and link-local ranges are dropped), announces itself on UDP multicast
`239.255.75.19:7519`, and serves agents on `http://127.0.0.1:7520/mcp`.

Peers must be able to reach TCP and UDP 7519 through this device's firewall;
`messh doctor` checks that (see [Network setup](#network-setup)).

On an always-on Linux device, you can optionally run it as a systemd user service.
Place the executable at `~/.local/bin/messh` for this example (or adjust
`ExecStart` to your installed path), create `~/.config/systemd/user/`, and save:

```ini
# ~/.config/systemd/user/messh.service
[Unit]
Description=messh node
After=network-online.target

[Service]
ExecStart=%h/.local/bin/messh node
Restart=on-failure

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now messh
loginctl enable-linger "$USER"   # keep it running without a login session
```

Linger may require administrator permission. This service configuration is for
headless work; it does not create a desktop session for approval prompts.

### Update in place

```sh
messh version --json
messh update --check --json
messh update --json
```

`--check` reports `current`, `available`, `update_available`, and `reason`. It is
read-only: it does not create state, download an executable, lock an installation,
or stop a node. No published main build and an already-current build are successful
no-ops. Checks use public GitHub APIs without requiring a GitHub login; network
failures and GitHub's unauthenticated rate limits are reported as errors.

`messh update` explicitly opts this installation into the **main channel**, including
when invoked from a source build or tagged release. It selects the greatest
published main build number, not the most recently edited release or GitHub's
stable-only `releases/latest` endpoint. A newer installed main build is never
downgraded. A source build at the same commit is not treated as the published build.
There are no background upgrades, arbitrary-source flags, or force-update option.
Only the updater contacts GitHub; mesh traffic remains LAN-only.

Installation downloads and checks the complete release metadata, SHA-256 checksums,
archive layout, and staged executable's embedded identity **before stopping anything**.
It replaces the executable at its existing path and retains `<executable>.previous`.
Pairings, identity keys, agent tokens, approvals/rules, jobs, files, service
configuration, and firewall paths are preserved. Only the executable is updated;
the separately installed pi extension is not upgraded by this command.

A running node must use a recognized existing per-user launcher: `messh.service`
under Linux `systemd --user`, or the `messh` Scheduled Task in the current Windows
desktop session. The updater validates the launcher, executable, state directory,
and process ownership; it does not guess how to restart custom/unmanaged processes.
For an unsupported launcher, finish active work, stop it manually, update while it
is stopped, and restart it yourself. Updating a stopped installation does not start
a node. Symlink installations are refused rather than replacing an unexpected target.

The owner-authenticated local maintenance gate refuses **all nonterminal jobs,
including other agents' jobs and pending approvals**, active requests, model streams,
transfers, and claimed scheduled work. New work cannot race the idle check and stop.
Wait for work to finish or cancel your own job, then retry; never delete the lock file
or kill active work to bypass a refusal. The nonrenewable startup gate lasts two minutes.

Success requires a restarted node with the expected build and unchanged device ID.
A failed candidate startup or health check triggers bounded recovery through the
same launcher, restoring the previous executable and verifying the old node before
reopening admissions. Rollback still returns an error describing the failed update.
Recovery never rewinds user data. If process/launcher ownership changed or the gate
expired, recovery fails closed instead of killing an unverified process: retain the
`.previous` binary, staging directory, `update-startup.json`, and any
`update-host-recovery.json`, and inspect the reported failure and original launcher.
An explicit subsequent update can restore an interrupted, unchanged Windows task's
enabled setting from its owner-only recovery record; `--check` never does so.

**First updater-enabled install:** older binaries do not have these commands or the
maintenance API. Finish work, stop the old node and verify its actual process exited,
then install a verified release executable (or build this checkout) at the same path
and restart the original launcher. Do not remove its state directory. On Windows,
stopping a PowerShell task wrapper alone may leave its child node running; the child
must also exit before replacement. Subsequent upgrades use `messh update`.

### Pair devices

Pairing is two-sided and happens once per pair of devices. Each side proves a
person compared the code on both devices by typing the other side's code into
a flag. On device A:

```sh
messh pair accept
```

On device B (use a name from `messh discover`, an ID prefix, or `host[:port]`):

```sh
messh discover
messh pair desktop
# prints a code and an outgoing pairing ID, e.g. code 441-354, id abc123
```

On device A, list the incoming request and check the code matches the one B
printed:

```sh
messh pair requests
messh pair approve INCOMING_ID --code 441-354
```

Back on device B, confirm with the code A printed (from its `pair requests`):

```sh
messh pair confirm OUTGOING_ID --code CODE_FROM_A
```

Replace `INCOMING_ID`, `OUTGOING_ID`, and `CODE_FROM_A` with the values shown by
the corresponding node; `441-354` above is an example, not a fixed pairing code.

If the codes differ, the command fails and leaves the request pending so a typo
can be retried (`messh pair reject ID` / `messh pair cancel ID` to give up).
`messh peers` lists paired devices; `messh unpair NAME` forgets one (run it on
both sides).

### Connect agents

Register each agent on the device it runs on; the command prints its config:

```sh
messh agent add omp
```

**omp** — merge the printed block into `~/.omp/agent/mcp.json`:

```json
{
  "mcpServers": {
    "messh": {
      "type": "http",
      "url": "http://127.0.0.1:7520/mcp",
      "headers": { "Authorization": "!messh agent token omp --bearer" }
    }
  }
}
```

The leading `!` makes omp run the command and use its output as the header.
Don't name the server `browser` or `playwright`: omp silently drops servers it
mistakes for browser automation while its built-in browser is enabled.

**pi** has no built-in MCP client; messh ships a pi extension in
[`integrations/pi`](integrations/pi/README.md) that registers the node's tools as
native pi tools. Run `messh agent add pi`, then `pi install ./integrations/pi`
(copy the folder to devices without the source), and check with `/messh` in pi.

**Any other MCP client** — Streamable HTTP at `http://127.0.0.1:7520/mcp` with
`Authorization: Bearer <token>`. Both the 2026-07-28 protocol and the older
`initialize`/session protocols are accepted.

Each agent has a tool mode, shown by `messh agent ls`:

- `compact` (default) — the agent sees only the mesh tools: `mesh_nodes`,
  `mesh_tools`, `mesh_call`, and the other mesh-wide tools such as `mesh_copy`.
  It finds a device's tools with `mesh_tools` (search by words, returns
  descriptions and input schemas) and runs them with `mesh_call`. This keeps the
  agent's context small however many devices and services the mesh has.
- `full` — every device tool is listed directly as `<device>__<tool>`
  (`desktop__node_info`) next to the mesh tools; the list grows with enabled features and services.

Choose with `messh agent add NAME --tools full` (re-running `add` on an existing
agent keeps its token) or `messh agent mode NAME compact|full`; the change
applies from the agent's next request. Calls through `mesh_call` pass the same
approval gates as direct calls. `messh tools` and `messh call desktop__node_info`
show the full view from the command line. `messh agent rm NAME` revokes an
agent's token.

### Read-only smoke call

Once both nodes are running and paired, run these on the agent's device:

```sh
messh status
messh peers
messh tools
messh call desktop__node_info --json
```

Replace `desktop__node_info` with the exact remote tool name shown by
`messh tools`. The call reports that device's OS, CPU, memory, GPUs, and detected
runtimes; it does not start a job or require a write approval. A successful JSON
result has `is_error: false`. If the peer is offline, solve connectivity first
with `messh doctor`.

In omp or pi, ask the agent to call `mesh_nodes`, find the paired device's
`node_info` with `mesh_tools`, then call `mesh_call` with that device and tool.
This exercises the registered agent's token rather than the CLI's owner token.
Do not approve a job, browser action, or service call just to test pairing.

## Architecture

```text
agent (omp / pi / other MCP client)
    |
    | loopback HTTP + per-agent bearer token
    v
local messh node
    |
    | LAN TCP + mutually authenticated, pinned TLS 1.3
    v
paired messh node
    |
    +-- read-only machine information
    +-- workspace/artifact transfer
    +-- approval gate --> jobs / local services / models / browser
```

Discovery uses LAN multicast; it is only a hint, never an identity or trust
source. The local node brokers calls to paired nodes, while the target device
enforces its own approval policy. Job execution, service credentials, browser
profiles, and audit records stay on the device that owns them. Model responses
and transferred files travel over the mesh, not an external relay.

messh is LAN-only. There is no public rendezvous server, hosted relay, or
Internet-facing deployment mode. Do not port-forward its ports or expose the
local agent/control endpoints publicly.

## Using messh from an agent's shell

The CLI is agent-safe: no command ever waits for typed input, every command
takes `--json` (one line of JSON on stdout; errors are
`{"error":{"kind","message","hint"}}` on stderr), `messh describe --json`
describes every command as an OpenCLI 0.1 document, and the exit code says
what to do next: 0 ok, 1 failed (stderr may name the next command), 2 bad
usage, 3 no node running for this state directory (start one with
`messh node`), 4 a person must act first (relay the hint to the user).

Required options are marked `required: true` in `describe` and appear without
brackets in help; `pair approve` and `pair confirm` require `--code CODE`.
Missing or empty codes exit 2 before contacting the node. A missing `run.json`
or a refused connection to its local endpoint exits 3, including `tools` and
`call`; unreadable state and authentication failures remain ordinary failures.

Install the bundled skill so the agent knows all this:

```sh
messh skill install                  # omp: ~/.omp/agent/skills/messh/SKILL.md
messh skill install --for pi         # pi: ~/.pi/agent/skills/messh/SKILL.md
messh skill install --for agents     # plain layout: ~/.agents/skills/messh/SKILL.md
messh skill install --dir DIR         # custom directory; refuses to replace a different skill
messh skill show                     # print the SKILL.md text (--json: {name, content})
```

Agents must always pass `--json`, learn commands from `messh describe --json`
or `messh COMMAND -h` instead of guessing, and never run the destructive or
consent-bearing commands unless the user asked for that exact action:
`approvals allow|always|deny`, `rules rm`, `approvals unregister`,
`pair approve|confirm|accept`, `unpair`, `agent rm`, `agent token` (prints a
secret), `firewall allow|remove`, `browser reset-profile`, `service rm`. They
report pending approvals (`messh approvals --json`) to the user instead of
answering them, and never try to approve their own requests or route around
an approval.

## Network setup

```sh
messh doctor            # read-only: node, interfaces, firewall, multicast, peers; prints a Fix: list
messh doctor --json
```

Each check is `ok`, `warn`, `fail` or `unknown` (not readable without admin/root,
or a tool timed out) with a one-line finding. It covers: the running node and the
address its mesh port is bound to (loopback-only is a fail); every LAN interface
with its IPv4 address and, on Windows, its network category; the firewall (Windows
Firewall profile state, allow rules for messh.exe or port 7519 on the active
profiles, block rules for messh.exe; on Linux ufw, firewalld or nftables);
joining `239.255.75.19` on each interface (on a loopback-bound throwaway socket
that sends nothing); whether announcements from other devices arrive; and every
paired peer's status. With the node stopped it still runs the OS checks.

**Windows.** Windows filters inbound traffic per network category, and a Wi-Fi
network you never marked as trusted is *Public*: the first time messh listens
there Windows either asks (dismissing the prompt makes it create *block* rules
for messh.exe) or silently drops peers. Fix both at once:

```sh
messh firewall allow --private "INTERFACE_ALIAS"   # replace with doctor's interface alias; one UAC prompt
messh firewall allow --dry-run            # print the exact netsh commands instead
messh firewall remove                     # delete messh's rules again
```

`allow` deletes the block rules Windows made for messh.exe and adds two inbound
rules for that program only, `messh (mesh TCP 7519)` and `messh (discovery UDP 7519)`,
on the Private and Domain profiles with remote addresses limited to `LocalSubnet`, so
even on a network you trust only devices on the same subnet get in. `--private`
marks that network Private (only use it for your own home or office network; Public
stays right for cafés and hotels) and is checked against the networks Windows
reports. Afterwards it re-runs the doctor's interface and firewall checks.

**Linux.** `messh firewall allow` prints the commands for the active firewall and
never runs sudo itself, for example:

```sh
sudo ufw allow from 192.168.1.0/24 to any port 7519 proto tcp
sudo ufw allow from 192.168.1.0/24 to any port 7519 proto udp
# firewalld:
sudo firewall-cmd --permanent --zone=public --add-rich-rule='rule family="ipv4" source address="192.168.1.0/24" port port="7519" protocol="tcp" accept'
sudo firewall-cmd --permanent --zone=public --add-rich-rule='rule family="ipv4" source address="192.168.1.0/24" port port="7519" protocol="udp" accept'
sudo firewall-cmd --reload
```

## Tools

In `compact` mode the `<device>__<tool>` rows below are not listed to the agent;
it calls them as `mesh_call {device: "desktop", tool: "node_info"}`.

| Tool | Returns |
|---|---|
| `mesh_nodes` | every device: name, ID, online, last contact, tools (full mode adds address and tool descriptions) |
| `mesh_tools {device?, query?, limit?}` | tool definitions (description, input schema, annotations) on one or all devices whose name or description contains every word of `query`; at most `limit` (default 20, max 100) plus the total matched |
| `mesh_call {device, tool, arguments?}` | runs `tool` on `device` (handle, name, or ID prefix) with `arguments` and returns the tool's own result |
| `<device>__node_info` | OS, machine, CPU, memory, GPUs with live VRAM, runtimes (python, uv, node, bun, go, git, docker, ollama, nvcc, ffmpeg), browsers, idle time and lock state |
| `<device>__catalogue` | the device's registered services: description, tags, up/down, latency, and which tools use each |
| `<device>__<service>__<tool>` | an MCP service's own tools, re-published (e.g. `desktop__voicestudio__generate_speech`) |
| `<device>__service_describe` / `service_call` | search an OpenAPI service's operations / make an HTTP request to a REST or plain HTTP service |
| `<device>__openai_models` / `openai_chat` | list models / non-streaming chat on an OpenAI-compatible service (Ollama, LM Studio, Unsloth Studio) |
| `<device>__job_submit` | run a program on the device as a background job (needs approval on that device); returns a `job_id` |
| `<device>__job_status` / `job_wait` / `job_logs` | state, exit code, queue position, output files (refs) and logs; block until done; follow stdout/stderr |
| `<device>__job_cancel` / `job_list` / `job_delete` | kill a job's whole process tree; list your jobs; remove a finished job and its workspace |
| `<device>__job_resources` | CPU threads, RAM, GPUs with VRAM total/free, and which are claimed by jobs |
| `mesh_copy` | copy a file or directory between devices (not per-device) |
| `mesh_wake {device, wait_seconds?}` | wake a sleeping device with Wake-on-LAN and wait (default 60 s, max 300) until it answers: state before (online/asleep/offline), awake, MACs used (partly masked), packets sent and routes, seconds to wake (not per-device, see Wake-on-LAN) |
| `mesh_schedule` / `mesh_schedules` / `mesh_unschedule` / `mesh_schedule_pause` / `mesh_schedule_run_now` | run a device's tool later or repeatedly, waking the device first; read the results afterwards (not per-device, see Scheduling) |
| `<device>__files_list` / `files_stat` / `files_mkdir` / `files_delete` | browse and tidy the device's `ws/` and `artifacts/` |
| `<device>__browser_navigate` / `browser_snapshot` / `browser_click` / `browser_type` / ... | drive a web browser on the device; each website needs the owner's approval (see Browser) |
| `<device>__browser_status` | whether the browser tools work on the device; never starts the browser |

## Services

The owner registers local services on the device that runs them; agents on other
devices find them with `catalogue` and use them through the tools above. Every
call except `catalogue` needs approval on the host device, unless you
pre-authorise it.

```sh
messh service scan                                  # find well-known local services (changes nothing)
messh service add voicestudio http://127.0.0.1:3900/mcp/ --desc "voice generation" --tag tts \
    --auto-tool check_health --auto-tool list_voices
messh service add unsloth http://127.0.0.1:8888 --kind openai --bearer sk-...   # key stays on this device
messh service ls
messh service show voicestudio
messh service rm voicestudio                         # only when you want to remove it
```

Kinds are detected automatically (`mcp`, `openapi`, `openai`, `http`). Only
loopback hosts are allowed unless the entry sets `allow_lan` (`--allow-lan`).
Entries live in `services.json` (owner-only, edited atomically by the CLI; a
running node picks up edits within seconds). Per entry: `auth` (header + value or
`value_file`, never shown to agents or logged), `auto` (`read`: REST GET/HEAD,
`openai_models`, MCP tools with `readOnlyHint`; `tools`: MCP tool names or
`"METHOD /path"`), and `allow`/`deny` lists (MCP tool names, or `"METHOD /path"`
with `*` for one path segment and a final `**` for the rest). Audio, images and
other binary results are saved as artifacts and returned as refs
(`artifacts/voicestudio/ab12cd.wav`) for `mesh_copy`; MCP arguments named `path`
or `*_path` accept `ws/...`/`artifacts/...` refs, translated to the host path.

## Use a model on another device

A model server on the desktop (Unsloth Studio, Ollama, LM Studio, anything
OpenAI-compatible) can be an ordinary model provider for agents on the Pi. Register
it on the desktop as an `openai` service (the API key stays there):

```sh
messh service add unsloth http://127.0.0.1:8888 --kind openai --bearer sk-...   # on the desktop
```

Every node then serves `http://127.0.0.1:7520/llm/<device>/<service>/v1` on loopback.
The API key is the agent's messh token (`Authorization: Bearer`, or `x-api-key`
for Anthropic-style clients). On the Pi:

```sh
messh llm ls                              # model services on the mesh and their local base URLs
messh llm config desktop unsloth          # omp: paste into ~/.omp/agent/models.yml
messh llm config desktop unsloth --for pi # pi: paste into ~/.pi/agent/models.json
messh llm config desktop unsloth --for openai --agent NAME   # base URL + key for any other client
```

The config uses `!messh agent token NAME` as the key, so the token is never written
into the harness's file; model IDs come from the service's `/v1/models` (or
`--model ID`). Requests stream over the mesh (server-sent events arrive as they are
generated; bodies up to 32 MiB) and closing the client stops the generation upstream.
Only `GET /v1/models` and `POST` to `/v1/chat/completions`, `/v1/completions`,
`/v1/embeddings`, `/v1/responses` and `/v1/messages` are forwarded; everything else
is 404. A service URL that already ends in `/v1` (Ollama's `http://127.0.0.1:11434/v1`)
is fine. Client `Authorization`, `x-api-key` and cookies are never forwarded; the
desktop adds its own key and strips it from error bodies.

The desktop's owner approves each request like any other service call: the prompt
names the agent, device, service, endpoint, model and whether it streams, never the
prompt text. "Always allow" offers *this model*, *any model on this service*, or
*anything on this service* (broad); listing models is allowed without asking when
the entry sets `auto.read`. A request waits while the prompt is open (up to 10
minutes), so keep client timeouts at least that long (the OpenAI SDKs default to 10
minutes); a client that retries after a timeout asks again. Denied requests get 403
with an OpenAI-style error, an unreachable device 502 (wake it with `mesh_wake`).
The audit log records status, duration and byte counts of each request, no content.

## Jobs

`job_submit` runs `command` + `args` (or a `shell:true` command line) on the host as
the logged-in user, inside the workspace `ws/<workspace>` (default: a new one named
after the job). The owner approves each submission on the host with the resolved
executable, arguments, environment, resource claims and the SHA-256 of every
`inputs` file; "always allow" can cover that exact command, one executable, or
everything from that agent. The call returns at once; the job waits for the
decision, then for its resources, and starts by itself.

- `resources`: `gpus:[0]` is exclusive per GPU (and sets `CUDA_VISIBLE_DEVICES`);
  `vram_mb` is checked against free VRAM and `mem_mb` against free RAM when the job
  would start; `cpus` reserves scheduler threads. The queue is FIFO per resource: a
  CPU-only job is never stuck behind a waiting GPU job, but GPU jobs keep their order.
  `mem_mb` is also a hard limit (Windows job object, Linux `systemd-run --user
  --scope`) where the OS allows; `job_status` notes when it is not enforced.
- `inputs` (`ws/…`, `artifacts/…` refs already on the host) are hard-linked (or
  copied) into the workspace so later overwrites cannot change what was approved.
  Outputs are the other files in the workspace, returned by `job_status` as refs for
  `mesh_copy`.
- Cancel, `timeout_seconds`, and node exit kill the whole process tree (Windows job
  object; Linux process group + systemd scope). After a restart, jobs that were live
  are marked `interrupted`.
- The device is kept awake while a job runs (Windows `SetThreadExecutionState`, Linux
  `systemd-inhibit`). Running jobs' `job_status` notes and `job_resources` report
  `sleep_inhibit: held` or `unavailable: <reason>`: polkit refuses block inhibitors
  to sessions that are not active local ones (ssh, headless, WSL), and the device may
  then sleep during jobs.
- Logs keep the first 4 MiB and the most recent output of each stream. Nothing is
  deleted automatically; `job_delete` removes a finished job, its logs and its workspace.

## Browser

Agents can use a web browser on another device, including sites its owner is
signed in to. The engine is Microsoft's [Playwright MCP](https://github.com/microsoft/playwright-mcp)
(pinned to `@playwright/mcp@0.0.83`, run through `npx`, so Node.js 18+ is
required); messh puts its per-website approval in front of every call. Nothing is
published until the owner runs, on the device with the browser:

```sh
messh browser setup --mode profile --channel chrome
messh browser status      # Node, Playwright MCP, browser path, does it start (opens no window)
```

Two modes:

- **`profile`** (recommended): a dedicated browser profile only agents use, at
  `<state>/browser/profile`. `messh browser login` opens it as an ordinary browser
  window (no automation) so you can sign in to the sites agents should use, once.
  `messh browser reset-profile --yes` deletes it. The first call after a start takes a couple of
  seconds (more if npx must download Playwright); a start still running after 25 s
  answers with a "still starting, retry" error instead of blocking.
- **`extension`**: attach to your running everyday browser through the
  [Playwright Extension](https://chromewebstore.google.com/detail/playwright-extension/mmlmfjhmonkocbjadbfplnigmagldckm).
  Without a token, each new agent session opens a page in your browser where you
  approve the connection and pick the tab to share (messh gives up waiting after 90 s with
  instructions). To skip that page copy `PLAYWRIGHT_MCP_EXTENSION_TOKEN` from the
  extension's status page and run `messh browser setup --mode extension --token -`. Your
  browser must already be running; `profile_dir` selects a profile such as `Profile 1`.

The browser starts on the first call, stops after `idle_timeout` (15 minutes) and when the
node stops, with every process it spawned (Windows job object, Linux process group). It
listens on a random `127.0.0.1` port and answers only requests addressed to that exact
host and port, at an unguessable URL.

**Approval per website.** Every call is classified by website (scheme, host, port;
subdomains are different sites) and level: *read* (snapshot, screenshot, console, network,
wait, tab list), *interact* (click, type, fill, select, press, hover, drag, dialogs, back)
and, separately, *upload* and *script*. Navigating asks for the target site at read level.
The prompt names the site, the action, the page address without credentials or query,
and which browser; "Always allow" offers *this site, view only*, *this site, view and
interact* (a saved interact rule also covers later reads) or *any site* (broad). Uploads
and scripts have their own rules that a click rule does not cover. Only `http` and
`https` pages work; `file:`, `chrome:`, `about:`, `devtools:`, `chrome-extension:`,
`view-source:`, `data:`, `javascript:` and the rest are refused outright. If messh cannot tell
which site the browser is showing (an unreadable tab list, a title that imitates the list
format), it refuses rather than guesses.

After any call that can change the page (a click, a redirect, a popup, back) messh looks at
where the browser ended up *before* returning any content. If a tab is on a site that is not
approved, a second approval for that site is requested through the same gate, mid-call (so
it prompts, honours saved rules and is audited like any other); the result is held back
until you answer. Deny, and the tab is reset to a blank page (an extra tab is closed) and
the agent gets only a refusal. The tab list the agent sees shows sites only, never titles.

```sh
messh browser allow github.com       # view without asking; interacting still asks. *.example.com covers subdomains
messh browser deny bank.example      # never reachable, no prompt
messh browser ls
messh browser rm github.com          # remove the saved site rule
```

`browser.json` (owner-only, reloaded within seconds): `enabled`, `mode`, `channel`
(`chrome`|`msedge`), `executable` (Brave, Chromium), `profile_dir`, `command` (default
`npx -y @playwright/mcp@0.0.83`; string or array), `headless`, `idle_timeout`,
`allow_origins`, `deny_origins`, `allow_script` (publishes `browser_evaluate`; default off),
`extension_token`. Only an allowlist of Playwright's tools is published: never
`browser_run_code_unsafe`, install, config, network interception, storage or devtools.
Screenshots and PDFs are saved as `artifacts/browser/ab12cd.png` and returned as refs (small
images are also returned inline). `browser_file_upload` takes `ws/...` or `artifacts/...`
refs only. A `filename` argument is refused.

This is human approval, not a sandbox: a page you approve can still send what is on it
anywhere, runs with your network access, and a redirect chain is judged by where it ends.
Register messh in omp under a name other than `browser` or `playwright` (for example
`messh`): omp silently drops MCP servers with those names while its built-in browser is on.

## Approvals

Every tool that is not read-only passes a gate on the device that would run
it. The gate builds a hash of the whole request (executable, arguments,
folder, environment, resources, input file hashes; or service, operation and
arguments), checks the saved rules, and otherwise asks the person at that
device with a **desktop notification**:

- **Allow once**: this request only.
- **Always allow**: saves a rule for the narrowest scope, *exactly this
  request* (the notification says so). It applies to that calling device and
  agent only.
- **Deny**.
- **More options** (or clicking the notification): opens a dialog for the same
  request with every scope to choose from, narrowest first, broad ones marked,
  plus full details. Closing that dialog without choosing leaves the request
  pending.

Closing or swiping away the notification does not answer anything: the
request stays pending until you answer it (`messh approvals`, or the
notification again) or it expires after 10 minutes. Several requests show as
several notifications at once. Try the real thing without risk:

```sh
messh approvals test      # shows a harmless test request, prints what you chose; saves nothing, runs nothing
messh approvals test --timeout 30s   # wait at most 30s for the answer (default 2m)
```

| Platform | Prompt, in order of preference |
|---|---|
| Windows | toast notification (bottom right, stays until acted on) → native task dialog → `messh approvals` |
| Linux | desktop notification via `notify-send` → `zenity` → terminal prompt (when the node runs in a terminal) → `messh approvals` |
| no desktop (a Raspberry Pi over ssh) | terminal prompt when the node runs in one, otherwise requests wait for `messh approvals` |

A surface that cannot start hands over to the next one, and the node logs why.

```sh
messh approvals                     # what is waiting
messh approvals allow REQUEST_ID     # approve this request once
messh approvals always REQUEST_ID --scope 0   # save the narrowest rule
messh approvals deny REQUEST_ID      # deny instead of allowing
messh approvals test --timeout 2m
messh approvals unregister          # Windows: remove messh's toasts and registry entries
messh rules                         # list saved "always allow" rules
messh rules rm RULE_ID               # remove a saved rule
messh audit -n 50
```

Rules belong to one calling device + agent + class, so another agent making
the same request is still asked. The audit log records every decision (and
the surface it came from) and the outcome of every call. Both live on the
device that enforces them.

### Windows toasts

Toasts are plain Windows notifications, shown by the node through the Windows
Runtime (no extra software). Their buttons open `messh-approve://` links, so
they keep working after the banner has moved to Action Center, and the node
need not be the one receiving the click. To make that work the node registers,
under your user only (`HKCU`, no administrator rights), the first time it
shows a toast, and re-checks before every toast:

| Key | Purpose |
|---|---|
| `Software\Classes\AppUserModelId\messh` | the "messh" name notifications appear under |
| `Software\Classes\messh-approve` | the link handler: `conhost.exe --headless "<messh.exe>" respond --notify "%1"` (`--headless` keeps the console-subsystem `messh.exe` from flashing a window; `--notify` shows a small message if the answer cannot be delivered); the path is updated if you move messh |

If the registration cannot be written or verified, or Windows has
notifications turned off for messh, the node falls back to the task dialog.
Toasts are removed when their request is answered anywhere (CLI, dialog,
timeout) and when the node shuts down; `messh approvals unregister` removes
the toasts first and then both keys. A click whose request is gone, or whose
node has stopped, shows one small message box instead of failing silently.

The click is authenticated by a random one-time token that exists only for
that request (inside the toast), not by the control token: it can answer that
one request and do nothing else. Windows' **Focus Assist / Do Not Disturb**
may silence the banner; the toast then waits in Action Center (expiry is the
request's 10 minutes), and `messh approvals` always works.

### Linux notifications

Needs `notify-send` from libnotify 0.8 or newer and a notification daemon
that advertises the `actions` capability (mako, dunst, swaync, KDE and GNOME
all do), plus `busctl` (systemd) or `gdbus` (glib) to check that and to close
notifications. If any is missing, messh uses `zenity`, then the terminal. The
notification is critical and stays until answered. The buttons are the same;
clicking the notification body (*Details*) or *More options* opens the zenity
dialog for the same request. The daemons differ in how they show actions:

| Daemon | Buttons |
|---|---|
| swaync, KDE Plasma, GNOME Shell | clickable buttons |
| dunst | left-click runs *Details*; the actions are in the context menu (`ctrl+shift+.` by default) or on middle-click |
| mako | only the default action is clickable (*Details*); invoke the others with `makoctl invoke once`, `makoctl invoke always`, `makoctl invoke deny` (or `makoctl menu -- wofi -d`) |

Native Linux desktop notification behavior has not been verified on a real
notification daemon. The notification paths are tested with stand-ins for
`notify-send`, `busctl`, and `gdbus`; Hyprland zenity placement is best effort
and has not been verified on a real Hyprland desktop.

## Files

Two directories inside the state directory are shared between devices: `ws/`
(working files for jobs) and `artifacts/` (outputs messh captured). Files are
named by refs such as `ws/render/scene.blend` or `desktop:artifacts/voicestudio/ab12cd.wav`
(device prefix optional). Paired devices stream files over the pinned mesh
connection, never through MCP payloads:

- `mesh_copy {from, to, overwrite?}`: file or directory, local to remote, remote to
  local, or remote to remote (relayed). SHA-256 is verified end to end; a
  trailing `/` on `to` puts the source inside that directory.
- `<device>__files_list` / `files_stat` / `files_mkdir` / `files_delete`: browse
  and tidy a device's `ws/` and `artifacts/`.

Peers cannot write into `artifacts/`. Symlinks, junctions and special files
inside the two directories are never followed or served.

HTTP/2 upload rejections return their file error without waiting for the source
to finish streaming. The rejection path keeps late digest trailers from closing
the connection used by subsequent transfers.

## Scheduling

An agent can have a tool call run later or on a timetable, e.g. on the always-on
Raspberry Pi: "tonight at 02:00 run this GPU job on the desktop" or "every day at
07:00 generate the morning briefing audio on the desktop". The schedule is kept
by the node the agent talks to (the Pi), not by the target. When it is due, that
node wakes the target with Wake-on-LAN if it is offline (waits up to 3 minutes),
then calls the tool as the agent that made the schedule and keeps the result.

- `mesh_schedule {device, tool, arguments?, at | in | every | daily, weekdays?, wake?, label?}`
  returns `{id, next}`. Exactly one of: `at` (RFC 3339, or `YYYY-MM-DD HH:MM` in
  the scheduling device's local time), `in` (Go duration, at least `10s`),
  `every` (Go duration, at least `1m`; elapsed time, first run one interval from
  now) or `daily` (`HH:MM` local wall-clock time, follows daylight saving;
  `weekdays: ["mon", ...]` restricts the days). `wake` defaults to true. The tool
  must be in the device's tool list (for an offline device, the list it last
  reported; the result then carries a warning).
- `mesh_schedules {id?}`: your schedules with their next time and last result,
  or one schedule with its arguments and its last 20 runs (results up to 64 KiB
  each). `mesh_unschedule {id}`, `mesh_schedule_pause {id, paused}`,
  `mesh_schedule_run_now {id}` (an extra run; returns at once).
- Agents only see and change their own schedules (at most 200 each). The owner
  sees all of them:

  ```sh
  messh schedule ls
  messh schedule show SCHEDULE_ID --json
  messh schedule pause SCHEDULE_ID
  messh schedule resume SCHEDULE_ID
  messh schedule run SCHEDULE_ID
  messh schedule rm SCHEDULE_ID
  ```

**Approvals still apply.** A scheduled call passes the target's approval gate
exactly like a direct call from the same agent, so unless a saved rule covers it
a prompt appears on the target device at run time, and the run fails if nobody
answers within the approval timeout. For runs while nobody is at the device,
make the same call once directly and choose "Always allow" (for jobs, a scope
that matches the scheduled command).

Up to 4 scheduled calls run at once, each limited to 10 minutes (long work
should be a `job_submit`, which returns at once; check it later with
`job_status`). If the scheduling node was off or asleep: a one-shot that is due
less than an hour ago runs once when it is back (marked missed), older ones are
recorded as missed without running; a repeating schedule records one missed run
and continues at its next future time. A fire less than 2 minutes late counts as
on time. Runs against a device that has since been unpaired fail with an error;
the schedule stays until removed. Calls to the scheduling device itself never
wake anything. Arguments are stored as given in `schedules.json` and are not
written to the log.

## Wake-on-LAN

Only some devices are always on. Agents wake the others before using them with
`mesh_wake {device}`, the owner with `messh wake DEVICE [--wait 60s]`; both
return at once for a device that is online. Tool calls to a device that said it
is going to sleep fail fast with `desktop is asleep; call mesh_wake first`
unless auto-wake is on for it: `messh wake DEVICE --auto on|off` (default off)
makes every tool call to it, while it is asleep or offline, wake it first and
wait up to 60 s. `messh peers` and `mesh_nodes` show `asleep`.

Nodes learn each other's MAC addresses over the pinned mesh connection (never
from discovery packets), right after pairing and then at most every 10
minutes, so a device must have been reachable once since pairing before it can
be woken. Physical interfaces only: Hyper-V/WSL `vEthernet`, Docker, bridges,
VPN and tunnel adapters are skipped. A wake sends 102-byte magic packets for
every known MAC of the device (Ethernet and Wi-Fi) to the subnet broadcast of
each of its subnets that a local interface is on, and to 255.255.255.255 from
every local interface, on UDP ports 9 and 7, three times 100 ms apart; it
re-sends every 15 s and tries to reach the device every 2 s until the wait
ends. When no local interface shares a subnet with the device, only
255.255.255.255 is used and the result says so.

What the target needs:

- Wake-on-LAN enabled in the BIOS/UEFI (often "Power On By PCI-E", "Wake on
  LAN"), and for Ethernet usually "ErP"/deep sleep off so the NIC keeps power.
- **Windows**: Device Manager → the network adapter → Power Management: tick
  "Allow this device to wake the computer" and "Only allow a magic packet to
  wake the computer" (without the latter, other traffic aimed at the NIC can
  wake the PC); Advanced: "Wake on Magic Packet" enabled. Waking from
  shutdown additionally needs Fast Startup off; waking from sleep does not.
- **Linux**: `sudo ethtool -s IFACE wol g` (lost at reboot), or persistently
  with NetworkManager `nmcli connection modify CONN 802-3-ethernet.wake-on-lan magic`,
  or a systemd `.link` file with `WakeOnLan=magic`.
- **Wi-Fi** (WoWLAN): driver and firmware support varies. It generally requires
  a supported sleep state with WoWLAN armed, a retained Wi-Fi association, and
  an access point that forwards broadcasts to sleeping clients. Do not assume
  it works from shutdown, hibernation, or Modern Standby. Wake-on-WLAN has not
  been verified on real hardware; Ethernet is the more reliable option.

**Sleep announcements.** Just before a device suspends, its node announces it:
a discovery multicast flagged `sleeping` plus an authenticated
`POST /v1/going-to-sleep` to every peer in contact, within about 1.5 s (Windows:
`PowerRegisterSuspendResumeNotification`; Linux: logind `PrepareForSleep` read
with `gdbus monitor` or `dbus-monitor`, with a `systemd-inhibit --mode=delay`
lock so the notice leaves before the suspend). Peers then stop their periodic
probes and tool-list refreshes to it until they hear it again (an awake
announcement or any contact from it) or wake it. That matters with
wake-on-pattern NICs, where every probe could wake the device or keep it
awake. A peer that sleeps without announcing (crash, lid closed while the
node was off) is probed with exponential backoff, up to every 10 minutes. The
multicast flag is an unauthenticated hint: a spoofed one can only delay
probes until the next real announcement or contact; only the authenticated
notice makes calls fail fast.

## Security model

- Each device has an Ed25519 key; its ID is the SHA-256 of the public key.
- Pairing requires an open window (`messh pair accept`) and a matching code
  confirmed by a person on each side; the code is derived from both IDs, so an
  interceptor presenting its own key produces different codes.
- Peers connect with TLS 1.3 client certificates pinned to the paired IDs;
  unpaired devices get 403. Discovery announcements are unauthenticated hints
  only — a device counts as online after a pinned connection succeeds.
- The agent endpoint (`/mcp`) and the model proxy (`/llm/...`) bind to loopback
  and require a per-agent bearer token. The node records which device and agent
  made every call. Approval buttons in notifications use a separate loopback
  endpoint that accepts only a single-use nonce for that one request.
- Anything that runs code (`job_submit`), acts through a local service (including
  model requests through the proxy), or drives the browser passes the host's
  approval gate, unless a saved rule or configured automatic policy permits it
  (see [Approvals](#approvals) and [Services](#services)). Paired devices can read
  `ws/` and `artifacts/`, write `ws/`, fetch Wake-on-LAN addresses, and send sleep
  notices to each other without interactive approval. Pair only trusted devices.
- **Same-user agents are fully trusted locally.** An agent running under the
  node owner's OS account can read its state directory, bearer tokens, control
  token, service credentials, and browser profile. It can use the owner's control
  API to change rules or answer approvals. A desktop prompt does not isolate that
  agent from the owner, and running an agent with that account is not a security
  boundary. The gates govern mesh requests; they do not sandbox jobs, services,
  browser content, trusted peer owners, or local processes.

Keep the state directory private and out of source control; backups contain
secrets and browser sign-ins. Use narrow saved rules, dedicated browser profiles,
and OS-level account isolation where needed. Revoking an agent with
`messh agent rm NAME` invalidates its token; unpair an unwanted device on both sides.

Report suspected vulnerabilities privately through
[GitHub private vulnerability reporting](https://github.com/dominic-codespoti/messh/security/advisories/new),
not a public issue. For ordinary bugs and feature requests, use
[GitHub issues](https://github.com/dominic-codespoti/messh/issues); redact tokens,
service credentials, browser cookies, prompts, and sensitive paths from reports.

## State

Per user: `%LOCALAPPDATA%\messh` on Windows, `$XDG_STATE_HOME/messh`
(`~/.local/state/messh`) on Linux, or `--state DIR` / `$MESSH_STATE`.

| File | Contents |
|---|---|
| `identity/key.pem`, `identity/cert.pem` | device key and self-signed certificate |
| `peers.json` | paired devices, last good addresses, cached tool lists |
| `agents/<name>.token`, `agents/<name>.json` | agent bearer tokens; tool mode (`{"tools":"compact"\|"full"}`, absent = compact) |
| `control.token` | token the CLI uses for the local control API |
| `config.json` | device name |
| `jobs/<id>/job.json`, `stdout.log*`, `stderr.log*` | job record (owner-only) and captured output |
| `ws/<workspace>/` | job working files and outputs; also where `mesh_copy` puts incoming files |
| `artifacts/<source>/` | outputs messh captured from services (audio, images), read-only to peers |
| `rules.json` | saved "always allow" rules (owner-only) |
| `audit.jsonl` | one record per approval decision and per completed call (rotates at 10 MB) |
| `services.json` | registered local services, including their credentials (owner-only) |
| `browser.json` | browser control settings: mode, channel, allowed/denied sites (owner-only; may hold the extension token) |
| `schedules.json` | scheduled calls of this node's agents: arguments and the last 20 results of each (owner-only) |
| `browser/profile/` | the agent-only browser profile in `profile` mode: its cookies and sign-ins |
| `browser/work/` | the browser's scratch folder (screenshots before they are saved as artifacts, staged uploads); wiped at each start |
| `run.json` | address of the running node, for the CLI |

## Limitations

- This is experimental, not a hardened multi-tenant platform. There is no claim
  of an independent security audit or production readiness.
- Supported targets are Windows amd64 and Linux amd64/arm64; macOS is unsupported.
- Discovery depends on multicast and peer traffic permitted by the LAN/firewall.
  Wi-Fi client isolation, guest networks, VLAN boundaries, VPNs, and WSL networking
  can prevent it. Direct-address pairing still requires TCP connectivity.
- Run the Windows node in the user's login session. Linux headless approvals need
  an owner to answer through the CLI, or an explicit saved rule for unattended work.
- Native desktop prompt behavior, particularly Linux/Hyprland, and physical
  Wake-on-WLAN remain unverified; passing unit tests is not hardware verification.
- Resource claims are scheduling policy, not general isolation. Linux hard memory
  limits and sleep inhibition depend on usable OS facilities and permissions;
  check job status notes rather than assuming enforcement.
- Browser control is optional and grants access to the approved profile's signed-in
  sites. It is human consent, not containment of web content or its network access.
- Jobs and their workspaces are not automatically deleted. Manage retained files
  with `job_delete` and the file tools; audit records and state can contain metadata.

## Troubleshooting

Start with `messh status`, `messh doctor`, and `messh doctor --json`. The doctor is
read-only and prints suggested fixes; review them before changing your firewall.

| Symptom | What to check |
|---|---|
| CLI exits 3 / no node running | Start `messh node`. Use the same `--state DIR` or `MESSH_STATE` for the node and CLI; check `messh status`. |
| Another device is not discovered | Check TCP/UDP 7519, the chosen LAN interfaces, multicast support, and Wi-Fi client isolation. Try `messh pair HOST:7519` during the other node's acceptance window. |
| Windows peers cannot connect | Run `messh doctor`; review active firewall profiles and stale executable block rules. Use [Network setup](#network-setup) only for a trusted network. |
| Paired peer remains offline | Both nodes must be running and reachable on the mesh port. Verify the address, firewall, and whether the device is sleeping; use Wake-on-LAN only after configuring the target. |
| Pairing requires a person / exits 4 | Run `pair requests`, compare the displayed codes on both devices, then explicitly approve and confirm. Never skip code comparison. |
| Agent gets HTTP 401 | Register it locally with `messh agent add NAME`; align its token, endpoint, and state directory. Do not share tokens between devices. |
| Tool names are missing | Compact mode exposes mesh tools rather than every per-device tool. Use `mesh_tools`; refresh pi with `/messh` after pairing or adding services. |
| omp does not show messh's tools | Name the MCP server `messh`, not `browser` or `playwright`; confirm the printed config and token command work. |
| A call waits for approval | Check `messh approvals` on the **target** device and its desktop notifications. Only the owner should allow it; unanswered requests expire after 10 minutes. |
| Linux notifications or memory/sleep integration are unavailable | Check the optional dependencies and session permissions. CLI approvals still work; job notes state what could not be enforced. |
| Browser tools do not appear | Run `messh browser setup`, then `messh browser status`; check Node.js/`npx`, the browser, and any extension attachment prompt. |
| pi cannot connect | See the [pi troubleshooting table](integrations/pi/README.md#troubleshooting). |

The CLI's authoritative command/flag reference is `messh -h`,
`messh COMMAND -h`, and `messh describe --json`.

## Development

From a checkout, with the Go version required by `go.mod`:

```sh
go test ./...
go vet ./...
go build ./cmd/messh
```

Tests cover the protocol, policies, jobs, transfer, and platform adapters; they
do not prove desktop notification behavior or Wake-on-LAN on physical hardware.
For a manual mesh smoke check, run nodes on two devices, follow
[pairing](#pair-devices), and make the [read-only remote call](#read-only-smoke-call).
Keep test state separate from your real node with `--state DIR`.

For the pi extension, use **Node.js 24** (the test runner executes `.ts` directly):

```sh
cd integrations/pi
npm ci
npm run check
npm test
```

The committed npm lockfile makes development and CI dependency installation
reproducible. See [pi development notes](integrations/pi/README.md#development)
for the extension layout and API compatibility.

## CI and releases

The [CI workflow](https://github.com/dominic-codespoti/messh/actions/workflows/ci.yml)
runs on pushes and pull requests:

- Native Go tests and vet on Ubuntu and Windows, using the Go version in `go.mod`.
- pi extension type checking, tests, and a high-severity dependency audit on Node.js 24 with `npm ci`.
- Workflow validation with actionlint and full Git history secret scanning with Gitleaks.
- Pure-Go archives for `windows-amd64`, `linux-amd64`, and `linux-arm64`, plus
  SHA-256 checksums, available as artifacts on a successful workflow run.

All verification jobs must pass before archives are built. Only a direct trusted
push to this repository's `main` branch publishes a main-channel prerelease.
Pull requests and manual runs do not publish main releases. SemVer `v*` tags
still publish tagged releases. Official Actions are pinned to commit SHAs; only
the gated publication jobs have repository write permission.

Main releases use tag `main-<build-number>-<12-character-commit>` and version
`0.1.0-main.<build-number>+<12-character-commit>`. The build number is the CI run
number. Publication verifies the successful run's provenance, full commit,
manifest, archive contents, and checksums; it uploads a complete draft before
making the release visible. Published releases are immutable: a rerun verifies
and reuses identical assets rather than overwriting them. An older build published
later does not become the updater's newest build.

`messh version --json` reports `{version,commit,channel,build}` without requiring a
running node. `messh status --json` exposes the same identity as `build`, and
`node_info` exposes it as `messh_build`. Ordinary source builds use the
`development` channel and build `0`, even when built from a published commit.
Release packaging sets `messh/internal/buildinfo.Version`, `.Commit`, `.Channel`,
and `.Build` through Go's `-X` linker flags; `scripts/release/package.py` implements
the shared packaging path for main and tagged releases.

The `release-assets` CI artifact and
[GitHub Releases](https://github.com/dominic-codespoti/messh/releases) contain
`messh-<version>-windows-amd64.zip`, `messh-<version>-linux-amd64.tar.gz`,
`messh-<version>-linux-arm64.tar.gz`, `update.json`, and `SHA256SUMS`.
Each archive includes the executable, README, MIT license, and pi extension under
`integrations/pi/`. The versioned manifest identifies every platform archive,
size, SHA-256 digest, expected executable member, and complete build identity.
`SHA256SUMS` covers the manifest and all three archives.

When downloading manually, verify the archive against the accompanying checksum
file before installation. The updater uses only this repository's public release
assets over HTTPS. Checksums detect corruption and mismatched assets; they do
**not** authenticate a compromised publishing repository or GitHub account.

## License

messh is licensed under the [MIT License](LICENSE). Third-party dependencies and
optional tools retain their own licenses.
