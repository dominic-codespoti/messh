# Architecture

Messh is a device-to-device system, not a hosted broker. Every participating device runs the same node process. Agent clients connect to their local node; nodes route authorized mesh calls to paired peers.

```text
agent / MCP client -- loopback HTTP + agent token --> local node
                                                        |
                                           pinned mutual TLS over LAN
                                                        v
                                                  paired node
                                                        |
                                    local policy, jobs, files, services
```

## Internal boundaries

- **Providers** describe and invoke local tools/services through a common provider contract. The node composes providers into a catalogue for local and remote discovery.
- **Node** owns peer identity/pairing, transport, MCP exposure, routing, local approvals, resource information, and the local execution boundary. The target node—not the caller—enforces its local policy.
- **Jobs** provide durable accepted requests, lifecycle state, logs, and optional application checkpoint/resume. See the [durability contract](jobs.md); this is not transparent process-memory recovery.
- **Schedule** stores future/repeating tool-call occurrences and dispatches pending work through the node's normal call policy. It is not a separate job execution environment.
- **Gateway** is the agent-facing MCP boundary: it exposes the local endpoint and routes authenticated tool requests into node functionality.
- **CLI** and explicit harness adapters live under `cmd/messh`; their responsibilities are presentation and configuration mapping, not duplicate mesh/job implementations. Optional native client integrations live under `integrations/`. See the [omp guide](../integrations/omp/README.md) and [pi guide](../integrations/pi/README.md).

This separation keeps transport identity, per-node policy, and job persistence at the devices that own them. It does not mean each layer is an independently deployable service.

## Trust boundaries

LAN multicast is discovery only and must not be trusted as peer identity. Pairing requires a person to compare codes out of band; subsequent peer connections authenticate pinned device keys over TLS. The agent-facing MCP listener is loopback-only and requires the registered agent's bearer token.

A node runs with its owner's operating-system privileges. Human approval gates are consent and policy checks, **not** process sandboxing; they do not isolate files, credentials, or effects from other processes under the same account. Use OS accounts, permissions, and firewall rules for isolation.
