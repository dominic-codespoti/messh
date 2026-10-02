# messh with pi

This extension connects [pi](https://github.com/earendil-works/pi/tree/main/packages/coding-agent) (`@earendil-works/pi-coding-agent`) to the messh node on the same device. It registers messh's MCP tools as native pi tools; pi does not need a separate MCP client.

It imports only pi types and Node built-ins, with no runtime npm dependencies or build step. You need pi, its supported Node.js runtime (the extension declares Node.js 20.6+; current pi requires Node.js 22.19+), and a working `messh` executable. **Node.js 24 is required for the development commands below**, including direct execution of `.ts` tests; it is not required just to run a messh node.

Read the [project quick start](../../README.md#quick-start), [security model](../../README.md#security), [limitations](../../docs/reference.md#limitations), and [job durability contract](../../docs/jobs.md) before connecting an agent. A pi process under the node owner's OS account can access that account's resources; approvals are not a sandbox.

## Install

On the device where pi runs, start the local node:

```sh
messh node
```

In a separate terminal, create an explicit agent identity:

```sh
messh agent add assistant
```

Set the extension's agent name to the same registered name. From a messh checkout or copied integration folder, install it with `pi install ./integrations/pi` or copy the whole folder to `~/.pi/agent/extensions/messh/`. For a temporary run, use `pi -e ./integrations/pi/index.ts`. Pair other devices using the [two-sided pairing instructions](../../README.md#pair-devices).

An installed local path must stay at that location. A copy must contain the complete integration directory. To install the CLI skill separately, use `messh skill install --for pi`; the skill does not replace this extension.

## Configuration

The extension defaults to the messh agent name `pi`; this pi-specific default is not an identity requirement imposed by messh. The chosen name must be registered on the local node. To use the install example's `assistant` identity, set `MESSH_AGENT=assistant pi` or add `{"agent":"assistant"}` to `~/.pi/agent/messh.json` (or `$PI_CODING_AGENT_DIR/messh.json`). Environment variables take precedence over file settings.

| Environment | File key | Purpose |
|---|---|---|
| `MESSH_AGENT` | `agent` | Registered agent name; defaults to `pi`. |
| `MESSH_URL` | `url` | Local node MCP endpoint (normally `http://127.0.0.1:7520/mcp`). |
| `MESSH_BIN` | `messh` | Executable used to run the token command; default is `messh` on `PATH`. |
| `MESSH_STATE` | `state` | Optional non-default state directory passed to token lookup. |
| `MESSH_TOKEN` | — | Optional literal bearer token; secret, so avoid persistent/shared files. |

Example for a node using a custom state directory:

```json
{ "agent": "assistant", "state": "/path/to/messh-state", "url": "http://127.0.0.1:7520/mcp" }
```

Normally the extension executes `messh agent token NAME --bearer [--state DIR]` directly (no shell, 15-second timeout); it does not log or show the token. Set `MESSH_BIN` to an executable, not a `.cmd`/`.bat` wrapper. Treat `MESSH_TOKEN` as a credential; do not commit it or include it in prompts/diagnostics. The URL must point to the local node, never directly to a peer.

## Verify and operate

Start pi and run `/messh` to connect/refresh tools, then call `mesh_nodes`, find a paired device, query `mesh_tools` for `node_info`, and invoke `mesh_call` with the actual device name and tool. This is a read-only check. In full mode, a node tool may also be called directly by its listed `<device>__<tool>` name.

The extension maps MCP tools to pi tools. Text and images are retained; binary/audio resources are represented by a note. Oversized text is saved under the temporary `messh-pi` directory. Calls allow up to 330 seconds (`job_wait` up to 300); Escape aborts the HTTP request. `/messh` and `messh_refresh` reload the tool catalogue. If the node is unavailable at startup, the extension exposes a status/retry tool. A 401 response refreshes the token and retries once.

## Troubleshooting

- **Connection refused:** start the local node; check `messh status` and set `MESSH_URL` if it reports a non-default endpoint.
- **Agent not registered / token rejected:** set `MESSH_AGENT` to the exact registered name and align `MESSH_STATE` with the node's state directory.
- **Executable not found:** put `messh` on `PATH` or set `MESSH_BIN` to its full executable path.
- **No remote tools:** confirm pairing with `messh peers`, then run `/messh` to refresh.

## Use a remote model

On the model-host device, register its OpenAI-compatible service:

```sh
messh service add model http://127.0.0.1:11434/v1 --kind openai
```

On the device running pi, register the agent and request pi-specific configuration:

```sh
messh agent add assistant
messh llm config desktop model --for pi --agent assistant
```

Replace the service/device names and URL for your setup. Merge the generated provider under `providers` in `~/.pi/agent/models.json`. The generated API-key command obtains the token at request time; preserve it rather than copying a token into the file. See the [model proxy reference](../../docs/reference.md#use-a-model-on-another-device) for approvals, supported routes, and timeouts.

## Development

Use Node.js 24 and npm. From the repository root:

```sh
cd integrations/pi
npm ci               # install dev dependencies from the committed lockfile
npm run check        # tsc --noEmit against maintained pi 0.99.2 types
npm test             # Node 24 runs lib.test.ts directly
npm audit --audit-level=high
```

The integration uses maintained `@earendil-works/pi-coding-agent` **0.99.2** and `typebox` 1.x types through pi's public `ExtensionAPI`; only type imports reference pi, and the host supplies the runtime. The `brace-expansion` override and committed lockfile pin the development dependency to patched 5.0.12. The pi lock entry deliberately omits `hasShrinkwrap` so `npm ci` does not reinstate upstream's 5.0.9 pin. When regenerating the lockfile, retain the patched resolution; `npm audit` checks lock metadata, not the installed tree.

The project's [CI workflow](https://github.com/dominic-codespoti/messh/actions/workflows/ci.yml) runs install, type check, and tests on Node.js 24. These checks validate extension types and transport/mapping helpers, not a real interactive pi session or native desktop approval behavior. The [read-only smoke call](#verify-and-operate) checks your actual setup.

The extension is part of messh and is covered by the project's [MIT License](../../LICENSE).
