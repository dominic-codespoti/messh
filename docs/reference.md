# messh reference

Detailed command, feature, network, and operational reference. Start with the [README quick start](../README.md#quick-start); harness-specific setup lives in [integrations/omp](../integrations/omp/README.md) and [integrations/pi](../integrations/pi/README.md).

## Requirements, platforms, and installation

Building requires Go 1.27.1 or newer, as declared in the repository's [go.mod](../go.mod). Core node operation is pure Go; optional features add their own dependencies.

| Platform | Supported target | Notes |
|---|---|---|
| Windows | `windows/amd64` | Run as the normal user in an interactive session for desktop approvals/browser; not a Windows service. |
| Linux | `linux/amd64`, `linux/arm64` | Desktop or headless; 64-bit Linux on Raspberry Pi hardware is supported. |
| macOS | Unsupported | No implementation or release target. |

From a source checkout, install with `go install ./cmd/messh`; add `GOBIN` (or `$(go env GOPATH)/bin`) to `PATH`. To build locally: `go build -o messh ./cmd/messh` (Windows: `go build -o messh.exe ./cmd/messh`). Cross-compile for 64-bit Raspberry Pi Linux from a POSIX shell with `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o messh-linux-arm64 ./cmd/messh`.

Optional requirements: browser control needs Node.js 18+, `npx`, a compatible Chromium browser and Playwright MCP; Linux desktop approval notifications need libnotify and an action-capable notification daemon (with `busctl` or `gdbus`; `zenity` is a dialog fallback). Optional systemd job memory/sleep integrations need their corresponding user services/tools. Jobs and local service programs must be installed separately.

### Optional headless Linux user service

For an always-on Linux node, place the executable at `~/.local/bin/messh` (or adjust `ExecStart`), create `~/.config/systemd/user/`, and save this unit as `messh.service`:

```ini
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
loginctl enable-linger "$USER"   # may require administrator permission
```

This is for headless operation; it does not create a desktop session for approvals.

## CLI and bundled skill

The CLI is non-interactive and supports `--json` for machine-readable results. Use `messh describe --json` or `messh COMMAND -h` for current command schemas and options rather than guessing. Exit codes: 0 success, 1 operation failed, 2 invalid usage, 3 no node running for the selected state directory, and 4 a person must act first. Pair approval/confirmation requires comparing the displayed short code on both devices.

A shell-capable agent can use the bundled skill, but installation has no implicit harness target: supply a destination directory, or see the explicit adapters in the [integration guides](../integrations/omp/README.md) and [pi guide](../integrations/pi/README.md).

```sh
messh skill install --dir "$HOME/.agents/skills"   # choose your destination
messh skill show
```

Commands that reveal secrets, grant access, or change pairing/approval policy require explicit human direction; agent guidance is not a substitute for owner review. See [Security](#security-model).

Persist a device name and non-default listens with `messh node config set`
(sparse updates, validated before save, restart to apply); updater launchers
stay `messh node --state ROOT`.

## Update in place

```sh
messh version --json
messh update --check --json  # read-only status check
messh update --json         # explicitly install the main channel
```

`messh update --check --json` reports `current`, `available`, `update_available`, and `reason`. It is read-only: it does not create state, download an executable, lock an installation, or stop a node. No published main build and an already-current build are successful no-ops. Checks use public GitHub APIs without requiring a login; network failures and GitHub unauthenticated rate limits are errors.

`messh update` explicitly opts this installation into the **main channel**, including source builds and tagged releases. It selects the greatest published main build number, not the most recently edited release or GitHub's stable-only `releases/latest` endpoint. A newer installed main build is never downgraded; a source build at the same commit is not treated as the published build. There are no background upgrades, arbitrary-source flags, or force-update option. Only the updater contacts GitHub; mesh traffic remains LAN-only.

Before stopping anything, the updater verifies release metadata, SHA-256 checksums, archive layout, and the staged executable's embedded identity. It replaces the executable at its existing path and retains `<executable>.previous`. Pairings, identity keys, agent tokens, approvals/rules, jobs, files, service configuration, and firewall paths are preserved. It updates only the executable; the separately installed pi extension is not upgraded.

A running node must use a recognized existing per-user launcher: `messh.service` under Linux `systemd --user`, or the `messh` Scheduled Task in the current Windows desktop session. The updater validates the launcher, executable, state directory, and process ownership; it does not guess how to restart custom/unmanaged processes. For an unsupported launcher, finish active work, stop it manually, update while stopped, and restart it yourself. Updating a stopped installation does not start a node. Symlink installations are refused rather than replacing an unexpected target.

The owner-authenticated local maintenance gate refuses **all locally hosted nonterminal jobs, including other agents' jobs and pending approvals**, active requests, model streams, transfers, and claimed scheduled work. New work cannot race the idle check and stop. Wait for work to finish or cancel your own job, then retry; never delete the lock file or kill active work to bypass a refusal. The nonrenewable startup gate lasts two minutes.

Live remote-outbox delivery and cancellation attempts also block maintenance. Durable submissions waiting between retries remain on disk without keeping the node permanently busy; maintenance pauses their delivery until admission reopens. Restarting the origin preserves those submissions and does not stop work already accepted by the target.

Success requires a restarted node with the expected build and unchanged device ID. A failed candidate startup or health check triggers bounded recovery through the same launcher, restoring the previous executable and verifying the old node before reopening admissions. Rollback still returns an error describing the failed update; recovery never rewinds user data. If process/launcher ownership changed or the gate expired, recovery fails closed instead of killing an unverified process. Retain the `.previous` binary, staging directory, `update-startup.json`, and any `update-host-recovery.json`; inspect the reported failure and original launcher. An explicit subsequent update can restore an interrupted, unchanged Windows task's enabled setting from its owner-only recovery record; `--check` never does so.

**First updater-enabled install:** older binaries lack the updater commands and maintenance API. Finish work, stop the old node and verify its actual process exited, then install a verified release executable (or build this checkout) at the same path and restart the original launcher. Do not remove its state directory. On Windows, stopping a PowerShell task wrapper alone may leave its child node running; the child must also exit before replacement.

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

**Linux.**

```sh
sudo ufw allow from 192.168.1.0/24 to any port 7519 proto tcp
sudo ufw allow from 192.168.1.0/24 to any port 7519 proto udp
# firewalld:
sudo firewall-cmd --permanent --zone=public --add-rich-rule='rule family="ipv4" source address="192.168.1.0/24" port port="7519" protocol="tcp" accept'
sudo firewall-cmd --permanent --zone=public --add-rich-rule='rule family="ipv4" source address="192.168.1.0/24" port port="7519" protocol="udp" accept'
sudo firewall-cmd --reload
```

### Windows + WSL desktop targets

Run one node per OS with separate identities and ports: `dompc-win` on the
default mesh/local ports, `dompc-wsl` in Ubuntu on mesh 7521/local 7522 with
its own state (`/home/dom/.local/state/messh`). Persist non-default listens
with `messh node config set` (updater launchers stay
`messh node --state ROOT`); expose WSL on the LAN with `messh wsl setup` and
inspect it with `messh wsl status [WINDOWS_DEVICE]`. The host route forwards
the LAN mesh port to the private guest IPv4 resolved from the default WSL2
NAT adapter, behind one narrow Private firewall rule for explicit peers; the
guest local API port is never forwarded or firewalled. A stopped guest is
confirmed only by successful host inventories, never by TCP alone.

Routing and status share a host-native resolver: it reads `Get-NetAdapter`
and the selected interface's `Get-NetNeighbor` inventory, never global ARP
or a previously captured guest IP. It requires exactly one Up adapter named
`vEthernet (WSL)` or `vEthernet (WSL (Hyper-V firewall))`, with exactly one
distinct eligible private IPv4 neighbor with a resolved unicast MAC address.
Missing or ambiguous observations abort route application before modifying
the proxy or firewall. This automatic route is for default WSL2 NAT, not
mirrored networking or renamed/custom virtual adapters. Neighbor-cache
observations do not authenticate the guest; pinned mesh TLS remains the
identity boundary.

Status marks a proxy `present` only when the captured listen address, both
mesh ports, and the currently resolved guest destination all match. A
plausible proxy with an unresolved guest is `unknown`, not proof of a valid
route. Reports include `current_guest_address` when observed and
`guest_address_error` when resolution fails; `guest_address_note` remains
informational about an optional stale capture. Status never starts WSL.

After updating a Windows binary, re-run `messh wsl setup` with the target
flags and approve its UAC prompt to replace the installed route script/task.
Updating the binary alone does not update installed PowerShell copies;
`messh wsl refresh` may reuse the existing secured task.

The one-shot elevated installer passes its fixed PowerShell body separately from
quoted staging-path literals (including paths with spaces or quotes); setup does
not execute staging files as privileged code. The protected route files remain
administrator/SYSTEM-owned.
## Tools

Tool visibility is configured per agent. The default `compact` mode lists mesh-wide discovery/call tools; the agent invokes a target tool through `mesh_call`. `full` mode also lists every target tool as `<device>__<tool>`. Manage it with:

```sh
messh agent ls
messh agent mode assistant full
messh agent mode assistant compact
```

The mode change applies on the agent's next request.

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
| `<device>__job_submit` | run a program on the device as a background job; returns a `job_id` |
| `<device>__job_status` / `job_wait` / `job_logs` | state, outputs, logs, and blocking status wait |
| `<device>__job_events` | owner-scoped lifecycle events with an opaque resumable cursor |
| `<device>__capability_list` / `capability_check` | caller-scoped grants and exact admission preview; does not execute or replace owner approval |
| `<device>__recipe_list` / `recipe_get` | discover owner-published immutable job recipes and typed parameter schemas |
| `<device>__job_cancel` / `job_list` / `job_delete` | kill a job's whole process tree; list your jobs; remove a finished job and its workspace |
| `<device>__job_resources` | CPU threads, RAM, GPUs with VRAM total/free, and which are claimed by jobs |
| `mesh_copy` | copy a file or directory between devices (not per-device) |
| `mesh_wake {device, wait_seconds?}` | wake a sleeping device with Wake-on-LAN and wait (default 60 s, max 300) until it answers: state before (online/asleep/offline), awake, MACs used (partly masked), packets sent and routes, seconds to wake (not per-device, see Wake-on-LAN) |
| `mesh_schedule` / `mesh_schedules` / `mesh_unschedule` / `mesh_schedule_pause` / `mesh_schedule_run_now` | run a device's tool later or repeatedly, waking the device first; read the results afterwards (not per-device, see Scheduling) |
| `<device>__files_list` / `files_stat` / `files_mkdir` / `files_delete` | browse and tidy the device's `ws/` and `artifacts/` |
| `<device>__browser_navigate` / `browser_snapshot` / `browser_click` / `browser_type` / ... | drive a web browser on the device; each website needs the owner's approval (see Browser) |
| `<device>__browser_status` | whether the browser tools work on the device; never starts the browser |

Desktop Linux work runs on the WSL target (`dompc-wsl`); native Windows
commands, services, and browser work run on the Windows target (`dompc-win`).
Selection is always explicit; budgets are per node and never aggregate across
Windows+WSL, which share one physical GPU. `desktop_targets` (via
`messh wsl status [WINDOWS_DEVICE]`) reports the captured target with
observed host/guest state and explicit recovery commands.

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

An OpenAI-compatible model server can be an ordinary model provider for clients on another device. Register it on its host as an `openai` service (the API key stays there):

```sh
messh service add unsloth http://127.0.0.1:8888 --kind openai --bearer sk-...   # on the model host
```

Every node then serves `http://127.0.0.1:7520/llm/<device>/<service>/v1` on loopback.
The API key is the agent's messh token (`Authorization: Bearer`, or `x-api-key`
for Anthropic-style clients). On the client device:

```sh
messh llm ls                              # model services on the mesh and their local base URLs
messh llm config desktop unsloth --for openai --agent assistant   # generic base URL + key
```

The generated configuration uses a runtime token command rather than writing the token into a file. Model IDs come from the service's `/v1/models` (or `--model ID`). For harness-specific model configuration, see the explicit integration guides. Requests stream over the mesh (server-sent events arrive as they are
generated; bodies up to 32 MiB) and closing the client stops the generation upstream.
Only `GET /v1/models` and `POST` to `/v1/chat/completions`, `/v1/completions`,
`/v1/embeddings`, `/v1/responses` and `/v1/messages` are forwarded; everything else
is 404. A service URL that already ends in `/v1` (Ollama's `http://127.0.0.1:11434/v1`)
is fine. Client `Authorization`, `x-api-key` and cookies are never forwarded; the
host adds its own key and strips it from error bodies.

The model host's owner approves each request like any other service call: the prompt
names the agent, device, service, endpoint, model and whether it streams, never the
prompt text. "Always allow" offers *this model*, *any model on this service*, or
*anything on this service* (broad); listing models is allowed without asking when
the entry sets `auto.read`. A request waits while the prompt is open (up to 10
minutes), so keep client timeouts at least that long (the OpenAI SDKs default to 10
minutes); a client that retries after a timeout asks again. Denied requests get 403
with an OpenAI-style error, an unreachable device 502 (wake it with `mesh_wake`).
The audit log records status, duration and byte counts of each request, no content.

## Durable jobs

A submitted job is durably queued and can be inspected by its stable ID. Node restart recovery and application-managed checkpoints are described in the [job durability contract](jobs.md). This does not replay arbitrary command processes or guarantee exactly-once external side effects. Use job status/logs/cancel/delete commands documented in the tool table below.

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
named by refs such as `ws/render/scene.blend` or `desktop:artifacts/voicestudio/ab12cd.wav`.
Named refs may include a device prefix. Files stream over the pinned mesh connection rather than MCP payloads. File access is deny-by-default: pairing alone grants none and there is no blanket peer migration. Finite path/action grants enforce list/read/write/copy/relay; peers cannot write `artifacts/`. See [capability guidance](jobs.md#capability-discovery-checks-and-owner-controlled-grants).

- `mesh_copy {from, to, overwrite?}`: file or directory, local to remote, remote to
  local, or remote to remote (relayed). SHA-256 is verified end to end; a
  trailing `/` on `to` puts the source inside that directory.
- `<device>__files_list` / `files_stat` / `files_mkdir` / `files_delete`: browse
  and tidy a device's `ws/` and `artifacts/`.

Peers cannot write into `artifacts/`. Symlinks, junctions and special files
inside the two directories are never followed or served.

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
written to the log. Each scheduled job submission uses a distinct durable request ID for its occurrence; redelivery of that job uses the same ID. This deduplicates job acceptance, not arbitrary tool side effects. See the [job delivery contract](jobs.md#safe-retry-keys-and-delivery).

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
- Anything that runs code (`job_submit`), acts through a local service, or drives the browser passes the host approval gate unless an owner-approved rule or policy permits it (see [Approvals](#approvals) and [Services](#services)). Owners may also create narrow, finite grants bound to device ID + agent label and exact tool request or file subtree/action. A grant check previews admission, not execution or independent human identity. Pairing alone grants no file access; there is no blanket migration. File access is deny-by-default for listing, read/write, copy, and relay; artifacts cannot be written. Expiry/revocation blocks new admissions but does not stop accepted work. Paired peers may exchange Wake-on-LAN addresses and sleep notices. Pair only trusted devices.
- A local agent label is authenticated by its bearer token. A remote agent label is an assertion made by the authenticated, trusted peer owner; it is not an independent end-to-end agent identity. Grants do not isolate a caller from its own peer owner or from processes sharing the host OS account.
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
| `grants.json` | finite owner-managed capability grants and revocations (owner-only) |
| `recipes.json` | immutable recipe definitions, schemas/digests, disabled status (owner-only, atomic; mode 0600 on POSIX, Windows ACL inherited from the state directory) |
| `jobs/event-tombstones.json` | durable owner-scoped lifecycle events for deleted jobs |
| `remote-jobs.json` | durable origin delivery receipts, credential-scoped cached status, and origin events |
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
| Tool names are missing | Compact mode exposes mesh tools rather than every per-device tool. Use `mesh_tools`; refresh or reconnect your client after pairing or adding services. |
| A call waits for approval | Check `messh approvals` on the **target** device and its desktop notifications. Only the owner should allow it; unanswered requests expire after 10 minutes. |
| Linux notifications or memory/sleep integration are unavailable | Check the optional dependencies and session permissions. CLI approvals still work; job notes state what could not be enforced. |
| Browser tools do not appear | Run `messh browser setup`, then `messh browser status`; check Node.js/`npx`, the browser, and any extension attachment prompt. |

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
[pairing](../README.md#pair-devices), then use the generic client setup in the [quick start](../README.md#connect-a-generic-mcp-client).
Keep test state separate from your real node with `--state DIR`.

For the pi extension, use **Node.js 24** (the test runner executes `.ts` directly):

```sh
cd integrations/pi
npm ci
npm run check
npm test
```

The committed npm lockfile makes development and CI dependency installation
reproducible. See [pi development notes](../integrations/pi/README.md#development)
for the extension layout and API compatibility.

## CI and releases

The [CI workflow](https://github.com/dominic-codespoti/messh/actions/workflows/ci.yml)
runs on pushes and pull requests:

- Native Go tests and vet on Ubuntu and Windows, using the Go version in `go.mod`. Windows runs test packages serially (`go test -p 1`) to reduce cold PowerShell startup contention. The ACL/exclusive-create fixture uses its existing 20-second test context for creation and security inspection; production launcher commands retain their separate 10-second bound.
- pi extension type checking and tests on Node.js 24 with `npm ci`.
- Workflow validation with actionlint and full Git history secret scanning with Gitleaks.
- Pure-Go archives for `windows-amd64`, `linux-amd64`, and `linux-arm64`, plus
  SHA-256 checksums, available as artifacts on a successful workflow run.

All verification jobs must pass before archives are built. Tagged release
publication verifies the archive checksums and that the tag still points to the
tested commit. Official Actions are pinned to commit SHAs; only the release
publication job has repository write permission.

Only a direct trusted push to this repository's `main` branch publishes an
immutable main-channel prerelease; pull requests and manual workflow runs do not
publish main releases. SemVer `v*` tags publish tagged releases. Main-channel
build metadata records the source commit and monotonically increasing build
number used by the updater; clients should not infer freshness from release
timestamps or tag versions.

Ordinary source builds report version `0.1.0-dev`. The release packaging script embeds the verified version, source commit, channel, and build number through `messh/internal/buildinfo` linker variables. Tagged builds derive their version from the verified SemVer tag; trusted main builds derive their identity from the tested commit and workflow run. Published archives and checksums are available from [GitHub Releases](https://github.com/dominic-codespoti/messh/releases).

The `release-assets` CI artifact contains `messh-<version>-windows-amd64.zip`,
`messh-<version>-linux-amd64.tar.gz`, `messh-<version>-linux-arm64.tar.gz`, and
`SHA256SUMS`. Each archive includes the executable, README, MIT license, and
the pi extension under `integrations/pi/`.

When downloading a build, compare its SHA-256 against the accompanying checksum
file before installation. CI artifacts are development snapshots, not tagged releases.

## License

messh is licensed under the [MIT License](../LICENSE). Third-party dependencies and
optional tools retain their own licenses.
