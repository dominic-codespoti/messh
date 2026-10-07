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
- **Node** owns peer identity/pairing, transport, MCP exposure, routing, local approvals, device-wide trust, resource information, and the local execution boundary. The target node—not the caller—enforces its local policy. Device trust is a separate, durable owner policy keyed by immutable peer device ID; it auto-approves all agents/actions from that device only on the node that records it. Unpairing revokes the local trust entry.
- **Jobs** provide durable accepted requests, lifecycle state, logs, and optional application checkpoint/resume. They also persist owner-scoped lifecycle events and deletion tombstones; the origin outbox persists its own delivery observations. Origin and target sequences have no cross-source total order. See the [durability contract](jobs.md); this is not transparent process-memory recovery.
- **Grants and recipes** provide narrow, expiring owner-delegated admission and immutable typed job definitions. Finite grants remain distinct from permanent device-wide trust: capability listing exposes grants only, while capability checks identify a trusted-device decision separately. Neither trust nor grants replace authentication or resource-boundary enforcement. Recipes resolve into the native job runner, not a second execution environment.
- **Schedule** stores future/repeating tool-call occurrences and dispatches pending work through the node's normal call policy. It is not a separate job execution environment.
- **Gateway** is the agent-facing MCP boundary: it exposes the local endpoint and routes authenticated tool requests into node functionality.
- **CLI** lives under `cmd/messh`; it provides generic node operations, MCP agent registration, and client-neutral configuration output, not a client-specific adapter layer.

This separation keeps transport identity, per-node policy, and job persistence at the devices that own them. It does not mean each layer is an independently deployable service.

## Trust boundaries

LAN multicast is discovery only and must not be trusted as peer identity. Pairing requires a person to compare codes out of band; subsequent peer connections authenticate pinned device keys over TLS. Any Streamable HTTP MCP client may connect to the loopback agent endpoint using an explicitly registered opaque agent identity and its bearer token in the Authorization header. Registration and authentication do not grant permissions; local owner policy still controls access. Device trust takes precedence over the ordinary approval gate on the node that stores it, for all agents/actions from that peer, but it does not bypass authentication, valid file references, workspace boundaries, or read-only artifact restrictions. It is permanent until removed or unpairing revokes it; it does not travel with the peer to another node.

A node runs with its owner's operating-system privileges. Human approval gates are consent and policy checks, **not** process sandboxing; they do not isolate files, credentials, or effects from other processes under the same account. Use OS accounts, permissions, and firewall rules for isolation.
