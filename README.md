# messh

messh connects Windows and Linux devices on a trusted LAN so an agent can use resources on another machine: tools, files, services, models, browsers, or background jobs. Each device runs a `messh node`; local clients connect over authenticated loopback MCP, and paired nodes communicate over pinned mutually authenticated TLS.

**Experimental software.** Start with read-only calls on devices you own. Approval is human consent, not an OS sandbox; commands and services run with the node owner's privileges. Review [Security](#security) before connecting an agent.

## Supported platforms

| Platform | Supported target | Notes |
|---|---|---|
| Windows | `windows/amd64` | Interactive user session for desktop/browser features; not a Windows service. |
| Linux | `linux/amd64`, `linux/arm64` | Desktop or headless, including 64-bit Raspberry Pi Linux. |
| macOS | Unsupported | No implementation or release target. |

Build with the Go version in [go.mod](go.mod). Core node operation is pure Go; optional feature requirements are in the [reference](docs/reference.md#requirements-platforms-and-installation).

## Quick start

Install from a source checkout on both devices:

```sh
go install ./cmd/messh
```

On the same trusted LAN, start a node on each device:

```sh
messh node --name desktop    # device A
```

```sh
messh node --name worker     # device B
```

Agents discover remote tool schemas before calls. File access remains subject to the target node's resource boundaries; pairing alone does not grant access. Owners may add finite path/action-scoped grants; see [capabilities and recipes](docs/jobs.md#capability-discovery-checks-and-owner-controlled-grants). An owner may separately add device-wide trust on a receiving node, which auto-approves every agent and exposed action from that paired device on that node only.
### Pair devices

Pairing is two-sided: a person compares a short code on both nodes. On device A, open the acceptance window:

```sh
messh pair accept
```

On device B, discover A and request pairing:

```sh
messh discover
messh pair desktop
```

Use the displayed IDs and actual codes to complete both prompts:

```sh
messh pair requests
messh pair approve INCOMING_ID --code CODE_FROM_B  # device A
messh pair confirm OUTGOING_ID --code CODE_FROM_A   # device B
messh peers
```

### Connect a generic MCP client

On the client device, create an explicit agent identity:

```sh
messh agent add assistant --json
```

The output from `agent add` has five fields: `agent`, `mode`, `created`, `mcp_url`, and `token_command`. Any Streamable HTTP MCP client can connect using the returned URL, registered opaque agent identity, and an `Authorization: Bearer <token>` header. Registration and authentication are explicit; they do not automatically grant permissions. Client configuration syntax is client-specific. Use secure secret management or a runtime command mechanism supported by your client; never publish or persist bearer tokens in shared configuration.

For example, a client that accepts URL and header settings could be configured with `mcp_url` as its endpoint and `Authorization` as a header whose value is `Bearer <runtime token>`. This describes the connection values, not a universal client configuration-file format or token-command extension.

Install the bundled skill by explicitly choosing its directory:

```sh
messh skill install --dir "$HOME/.agents/skills"
```

In default compact mode, use `mesh_nodes` to discover devices, `mesh_tools` to inspect schemas, and `mesh_call` to invoke a tool (for example, `node_info`). Full mode instead exposes target tools directly as `<device>__<tool>`. Both modes work with generic MCP clients; no adapter is required. Optional MCP Tasks negotiation is not required for ordinary jobs. The CLI also supports `messh status`, `messh tools`, and `messh call DEVICE__TOOL --json`.

## Durable jobs

Accepted jobs are persisted with stable IDs and recover after node restart. A running command is not automatically replayed after its node stops. Applications may opt into checkpoints and resume their own work; this cannot roll back external side effects or guarantee exactly-once execution. Read the [job durability contract](docs/jobs.md).

## Agent-first capabilities

- **Trusted devices:** messh trust ls, messh trust add DEVICE, and messh trust rm DEVICE manage permanent, revocable trust on the node where the command runs. Trust applies to every agent and exposed action from that device on that node, bypassing its normal approval prompt; it can allow commands or services to modify or delete the node owner's files. Unpairing revokes trust. This is not a sandbox or resource-boundary bypass: authentication, valid file references, workspace limits, and read-only artifact rules still apply. Keep this distinct from finite, agent-scoped grants.
- **Scoped grants:** agents discover their capabilities and check an exact request; owners issue finite, revocable tool or path/action grants. File access is denied by default, including on paired peers.
- **Typed job recipes:** discover an owner-published name/version/digest and parameter schema, then submit it through the native job API. Accepted snapshots and retry keys remain stable after disable or restart.
- **Durable events:** resume owner-filtered lifecycle events with opaque cursors, including origin delivery observations while a target is offline. Status remains authoritative; logs use separate byte offsets.

Use discovered MCP schemas rather than guessing calls. See the [agent capability and job workflow](docs/jobs.md#capability-discovery-checks-and-owner-controlled-grants); owner policy mutation is never an automatic agent action.

## Architecture and reference

The local agent talks to its node over loopback MCP. The node routes calls to paired devices, which execute them under their own approval and resource policy. LAN discovery is not an identity mechanism. See [architecture](docs/architecture.md) and the detailed [command and feature reference](docs/reference.md).

## Security

Peer connections use pinned, mutually authenticated TLS; the agent endpoint is loopback-bound and uses per-agent bearer tokens. Keep both endpoints private and do not port-forward them. Approval prompts are consent, **not** an OS sandbox: jobs, services, and browser actions run with the node owner's privileges. Device-wide trust is especially broad: every agent and exposed action from the trusted device is automatically approved on the node that stores that trust, and actions can modify or delete user files. Trust does not propagate to the other node and does not bypass authentication or resource boundaries. Use ordinary OS accounts, filesystem permissions, and network controls as the real containment boundary. Pair only devices you trust. See the [full security model](docs/reference.md#security-model).

## Troubleshooting

Start with `messh status`, `messh peers`, and the read-only `messh doctor`. Confirm both nodes are running, pairing codes matched, and TCP/UDP 7519 are reachable on a trusted LAN. Do not open public firewall rules to fix pairing. See [troubleshooting](docs/reference.md#troubleshooting).

## Development and license

Use the Go version in [go.mod](go.mod). Run `go test ./...`; use `go test -race ./...` for concurrency-sensitive changes. See the [development and release reference](docs/reference.md#development). messh is MIT-licensed; see [LICENSE](LICENSE).
