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

Agents discover remote tool schemas before calls. File access is deny-by-default: pairing alone does not grant files, and there is no blanket paired-peer migration. Owners may add finite path/action-scoped grants; see [capabilities and recipes](docs/jobs.md#capability-discovery-checks-and-owner-controlled-grants).
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

The result has exactly five fields: `agent`, `mode`, `created`, `mcp_url`, and `token_command`. Configure Streamable HTTP MCP with the returned `mcp_url`; run `token_command` at runtime and send its output as `Authorization: Bearer <token>`. Do not copy or persist the token. Client configuration syntax is client-specific; use the explicit [omp](integrations/omp/README.md) or [pi](integrations/pi/README.md) adapter guide where applicable. Generic messh setup has no implicit harness target. To install the bundled shell skill, choose its destination explicitly with `messh skill install --dir "$HOME/.agents/skills"`; harness-specific installs must likewise name their target (for example, `--for pi`).

Verify with a read-only call: discover devices with `mesh_nodes`, find tools with `mesh_tools`, and invoke `mesh_call` (for example, `node_info`). The CLI also supports `messh status`, `messh tools`, and `messh call DEVICE__TOOL --json`.

## Durable jobs

Accepted jobs are persisted with stable IDs and recover after node restart. A running command is not automatically replayed after its node stops. Applications may opt into checkpoints and resume their own work; this cannot roll back external side effects or guarantee exactly-once execution. Read the [job durability contract](docs/jobs.md).

## Agent-first capabilities

- **Scoped grants:** agents discover their capabilities and check an exact request; owners issue finite, revocable tool or path/action grants. File access is denied by default, including on paired peers.
- **Typed job recipes:** discover an owner-published name/version/digest and parameter schema, then submit it through the native job API. Accepted snapshots and retry keys remain stable after disable or restart.
- **Durable events:** resume owner-filtered lifecycle events with opaque cursors, including origin delivery observations while a target is offline. Status remains authoritative; logs use separate byte offsets.

Use discovered MCP schemas rather than guessing calls. See the [agent capability and job workflow](docs/jobs.md#capability-discovery-checks-and-owner-controlled-grants); owner policy mutation is never an automatic agent action.

## Architecture and reference

The local agent talks to its node over loopback MCP. The node routes calls to paired devices, which execute them under their own approval and resource policy. LAN discovery is not an identity mechanism. See [architecture](docs/architecture.md) and the detailed [command and feature reference](docs/reference.md).

## Security

Peer connections use pinned, mutually authenticated TLS; the agent endpoint is loopback-bound and uses per-agent bearer tokens. Keep both endpoints private and do not port-forward them. Approval prompts are consent, **not** an OS sandbox: jobs, services, and browser actions run with the node owner's privileges. Use ordinary OS accounts, filesystem permissions, and network controls as the real containment boundary. Pair only devices you trust. See the [full security model](docs/reference.md#security-model).

## Troubleshooting

Start with `messh status`, `messh peers`, and the read-only `messh doctor`. Confirm both nodes are running, pairing codes matched, and TCP/UDP 7519 are reachable on a trusted LAN. Do not open public firewall rules to fix pairing. See [troubleshooting](docs/reference.md#troubleshooting).

## Development and license

Use the Go version in [go.mod](go.mod). Run `go test ./...`; use `go test -race ./...` for concurrency-sensitive changes. See the [development and release reference](docs/reference.md#development). messh is MIT-licensed; see [LICENSE](LICENSE).
