// Package approval decides whether a tool call from another device or agent
// may run on this device.
//
// # Threat model
//
// The device that would run the action (the host) decides. A request
// arrives through the node's dispatch gate, which asks the provider to
// describe it (a provider.Approval), then asks the Engine. The Engine allows
// it only if
//
//   - the provider marked it Auto (the owner pre-authorised it in the
//     provider's own configuration),
//   - a saved rule for the same calling device, agent and class matches, or
//   - the person at the host answers a native prompt (Surface) with
//     "allow once" or "always allow".
//
// Prompts appear on the host and are answered there: through a native OS
// dialog, a terminal prompt on the node's console, or, on headless devices,
// through the loopback control API (messh approvals allow|always|deny). The
// calling agent has no way to answer: it never sees request IDs, and the
// control API needs the control token, which only the local user's CLI
// holds.
//
// The one other way to answer is a notification button. The Windows toast's
// buttons launch messh-approve:// URLs that carry the request's own one-time
// nonce (128 random bits, created per request, never listed by the control
// API); the node accepts it at POST /v1/approvals/{id}/respond without the
// control token (Engine.Activate). The nonce answers that one request and
// nothing else, and dies when the request is decided or expires.
//
// What this does not protect against: an agent that runs on the host itself
// as the same OS user can read the state directory (control token, rules
// file) and edit it, so it is exactly as trusted as the user there. The gate
// protects the host's resources from OTHER devices' agents, and from local
// agents only to the extent that they use the MCP gateway.
//
// Approvals are keyed by a hash of the whole request (Approval.Exact), so
// "allow once" for one command never covers a different one. "Always allow"
// saves a rule keyed by one of the provider's scopes (narrowest first, the
// exact request always offered) for the calling device and agent only.
// Unanswered requests time out as denied; timeouts, denials and
// cancellations are never saved.
//
// Rules live in Paths.RulesFile and every decision, plus the outcome of the
// call that followed, is appended to Paths.AuditFile. Prompts and audit
// records only contain what providers put into Approval (titles and details
// meant for a human); providers must not put secrets there.
package approval
