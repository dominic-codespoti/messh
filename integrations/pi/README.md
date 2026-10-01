# messh for pi

This extension connects
[pi](https://github.com/earendil-works/pi/tree/main/packages/coding-agent)
(`@earendil-works/pi-coding-agent`) to the messh node on the same device.
It registers messh's MCP tools as native pi tools, so an agent can discover
paired devices, inspect them, and use their jobs, local services, models,
browsers, and files. pi does not need a separate MCP client.

The extension imports only pi types and Node built-ins; it adds no runtime npm
dependencies or build step. You still need pi, its supported Node.js runtime
(the extension declares Node.js 20.6+; current pi requires Node.js 22.19+),
and a working `messh` executable.
**Node.js 24 is required for the development commands below**, including direct
execution of `.ts` tests; it is not required just to start a messh node.

messh is experimental and LAN-only. Read the project's
[quick start](../../README.md#quick-start),
[security model](../../README.md#security-model), and
[limitations](../../README.md#limitations) before connecting an agent. A pi
process running as the node owner's OS user can access the node's local
control/token state: host approvals are not a sandbox for that process.

## Contents

- [Install](#install)
- [Configure](#configure)
- [Read-only smoke call](#read-only-smoke-call)
- [What pi gets](#what-pi-gets)
- [Troubleshooting](#troubleshooting)
- [Implementation and compatibility](#implementation-and-compatibility)
- [Development](#development)

## Install

On the device where pi runs, first put `messh` on `PATH` and start its node
with `messh node`. Pair other devices using the
[two-sided pairing flow](../../README.md#pair-devices).

1. Register pi as an agent once: `messh agent add pi` (it prints the configuration steps).
2. Use `integrations/pi` from a checkout of
   [messh](https://github.com/dominic-codespoti/messh), from an extracted CI/release
   archive, or copy the **whole folder** to the agent's device.
3. Install it with either
   - `pi install ./integrations/pi` (or `pi install ~/messh-pi`): pi records the folder in
     `~/.pi/agent/settings.json` and loads it from there on every start; or
   - copy the folder to `~/.pi/agent/extensions/messh/`, which pi auto-discovers (and `/reload`
     reloads).

   To try it without installing: `pi -e ./integrations/pi/index.ts`.
4. Start pi and type `/messh`: it reconnects and shows the tools it registered. Or ask pi to call
   `mesh_nodes`.

The local-path install points at the folder; it does not copy it. Keep the folder
at that location. GitHub Releases contain archives only after a version tag is
published; a source checkout or successful CI run artifact works before that.

You may also install the bundled CLI skill with `messh skill install --for pi`.
The skill explains consent-sensitive CLI commands; it does not replace the extension.

## Configure

Nothing is needed for an agent named `pi` on a default node. Otherwise set environment variables or
create `~/.pi/agent/messh.json` (`$PI_CODING_AGENT_DIR/messh.json` if you moved pi's agent dir).
Environment variables win over the file, the file over the defaults.

| Variable | `messh.json` key | Default | Meaning |
|---|---|---|---|
| `MESSH_AGENT` | `agent` | `pi` | messh agent name (`messh agent add NAME`) |
| `MESSH_URL` | `url` | `http://127.0.0.1:7520/mcp` | the node's agent endpoint; `messh status` shows the real address |
| `MESSH_BIN` | `messh` | `messh` (on `PATH`) | messh executable used to fetch the token |
| `MESSH_STATE` | `state` | messh's default | passed to the token command as `--state` |
| `MESSH_TOKEN` | — | — | the literal token (with or without `Bearer `); skips the token command |

```json
{ "agent": "research", "state": "/path/to/messh-state", "url": "http://127.0.0.1:7530/mcp" }
```

The token is fetched by running `<MESSH_BIN> agent token <agent> --bearer [--state DIR]` directly
(no shell, 15 s timeout) and is never logged or shown. `MESSH_BIN` must be the executable itself
(`messh`, `/path/to/messh`, `C:\tools\messh.exe`), not a `.cmd`/`.bat` wrapper.

The URL must point to the **local** node, not directly to a paired device.
Align `MESSH_STATE` with that node if it uses a non-default state directory.
Treat `MESSH_TOKEN` as a secret; do not put it in version-controlled config,
prompts, or diagnostic reports.

## Read-only smoke call

After pairing and installation:

1. Run `/messh` in pi to connect and refresh its tools.
2. Ask pi to call `mesh_nodes` and identify the remote device.
3. Ask it to call `mesh_tools` with that device and query `node_info`.
4. Ask it to call `mesh_call` with `device: "desktop"` (replace with your
   paired device's handle/name), `tool: "node_info"`, and `arguments: {}`.

This returns the remote machine's information without submitting a job or
requiring a write approval. In full mode, pi can instead call the exact
`<device>__node_info` name shown in its tool list. For a CLI comparison, run
`messh tools` then `messh call desktop__node_info --json` with the actual tool
name; that CLI call uses the owner's control token, not pi's agent token.

## What pi gets

- **One pi tool per MCP tool**, named exactly as messh names it (`mesh_nodes`, `mesh_call`,
  `desktop__node_info`, ...), description prefixed `[messh]`, parameters = the tool's JSON Schema. In
  messh's default *compact* mode that is a handful of mesh tools (`mesh_nodes`, `mesh_tools`,
  `mesh_call`, ...); an agent in *full* mode gets every `<device>__<tool>`.
- **Results**: text stays text, images become pi images, audio and binary resources become a short
  note (type and size), resource links become a line with name, URI, type and size. Text beyond pi's
  limits (50 KB / 2000 lines) is cut; the full text is written to `<tmp>/messh-pi/` and its path is
  given in the result. Results with `isError` become failed pi tool calls carrying messh's message.
- **Timeouts and cancel**: tool calls may take up to 330 s (`job_wait` blocks up to 300 s); listing
  tools 15 s. Esc in pi aborts the HTTP request.
- **`messh_refresh`** (tool) and **`/messh`** (command) reload the tool list at runtime, e.g. after
  pairing a device or adding a service: new tools appear at once, changed ones are replaced, and
  tools messh no longer offers are deactivated (pi cannot unregister tools).
- **When messh is not reachable at startup** (node not running, unknown agent, wrong token, bad
  config) pi gets a single `messh_status` tool that explains the problem and retries connecting when
  called; on success the messh tools appear. Interactive pi also shows one warning with the fix, and
  `/messh` retries.
- A call rejected with HTTP 401 (for example after `messh agent rm` + `add` issued a new token)
  fetches the token again and retries once; nothing ran on the node in that case.
- MCP tools whose names collide with pi's built-ins (`read`, `bash`, `edit`, `write`, `grep`,
  `find`, `ls`) or this extension's own tools, or that are not valid pi tool names, are skipped with
  a warning. messh never produces such names; the guard is defensive.

## Troubleshooting

| Symptom (from `messh_status`, `/messh` or a failed call) | Fix |
|---|---|
| `cannot reach messh at http://127.0.0.1:7520/mcp ... ECONNREFUSED` | start the node (`messh node`, or `systemctl --user start messh`); if `messh status` shows another address set `MESSH_URL` |
| `` `messh agent token pi --bearer` failed: messh was not found `` | put messh on `PATH` or set `MESSH_BIN` to its full path |
| `` ... failed: agent "pi" is not registered `` | `messh agent add pi`, or set `MESSH_AGENT` to the registered name |
| `rejected the agent token (HTTP 401)` | the token belongs to another state dir or node: align `MESSH_STATE` and `MESSH_URL`, or re-run `messh agent add NAME` |
| `answered HTTP 404` | `MESSH_URL` must end in `/mcp` |
| a messh tool says it is no longer offered | the device or service went away; `messh_refresh` / `mesh_nodes` |
| no warning visible in `pi -p` | print and JSON modes have no UI; call `messh_status` |

pi loads extensions at startup: after editing `messh.json` use `/messh` (rereads it) or restart pi.

## Implementation and compatibility

The extension is type-checked against the maintained
`@earendil-works/pi-coding-agent` **0.99.2** and `typebox` 1.x. It uses pi's
public `ExtensionAPI` to register tools, commands, and session handlers.
Only type imports reference pi; the host supplies the extension runtime.
Future upstream API changes may require an update.

An offline RPC smoke run under pi 0.99.2 loaded the extension and executed
`/messh` against a real loopback messh node, discovering its ten mesh tools.
Interactive model sessions and native approval dialogs were not exercised.

messh side (Go MCP SDK v1.8.0, `mcp/streamable.go`): the agent endpoint is a stateless streamable
HTTP server answering JSON. For requests without `initialize` and a protocol version older than
2026-07-28 it synthesizes the session's initialize state (`ephemeralConnectOpts`), so the extension
sends `tools/list` and `tools/call` directly as JSON-RPC POSTs with `Accept: application/json,
text/event-stream` and `MCP-Protocol-Version: 2025-11-25`. It uses `node:http` rather than `fetch`
because undici's default 300 s headers timeout would cut off a long `job_wait`.

omp can load pi extensions too; this one uses no runtime pi APIs beyond the documented
`ExtensionAPI`, but it has only been checked against pi's types, not run under omp.

## Development

Use Node.js 24 and npm. From the repository root:

```sh
cd integrations/pi
npm ci               # install dev dependencies from the committed lockfile
npm run check        # tsc --noEmit against maintained pi 0.99.2 types
npm test             # Node 24 runs lib.test.ts directly
npm audit --audit-level=high
```

The `brace-expansion` override and committed lockfile pin the development
dependency to patched version 5.0.12. The pi lock entry deliberately omits
`hasShrinkwrap`: otherwise npm reinstates upstream's 5.0.9 pin during `npm ci`,
even when the root lockfile and override specify 5.0.12.

When regenerating the lockfile, retain the patched resolution and this metadata
change until upstream ships a patched shrinkwrap. After a clean install,
`npm ls brace-expansion --all` must report 5.0.12; `npm audit` alone checks lockfile
metadata and does not prove which version was installed. CI audits the whole
development dependency tree; these packages are not extension runtime dependencies.

`lib.ts` holds everything testable without pi (config, token parsing, the MCP client, schema and
result mapping); `index.ts` is the pi glue.

The project's [CI workflow](https://github.com/dominic-codespoti/messh/actions/workflows/ci.yml)
runs the same install, type check, and tests on Node.js 24. These checks validate
the extension's types and transport/mapping helpers, not a real interactive pi
session or native desktop approval behavior. Use the
[read-only smoke call](#read-only-smoke-call) to check your actual setup.

The extension is part of messh and is covered by the project's
[MIT License](../../LICENSE).
