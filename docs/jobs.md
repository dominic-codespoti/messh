# Native durable jobs

Use native job tools when work should run on a messh node in the background, survive node restarts, and expose durable status, events, logs, and outputs. The tools are job_submit, job_status, job_wait, job_list, job_events, job_logs, job_cancel, and job_delete, exposed remotely with a device prefix such as desktop__job_submit. The target owner must authorize every submission. A matching owner-approved rule or scoped capability grant may satisfy the approval gate without a new prompt; never route around the owner gate by retrying through another agent or route.

## Capability discovery, checks, and owner-controlled grants

Use `mesh_nodes` and `mesh_tools` to discover target tools and exact schemas; never guess names or arguments. `capability_list` shows only the caller’s grants. `capability_check` previews the grant decision for an exact tool+args or file path/action, and returns the native approval `args_hash` when applicable; it does not execute the request or replace other owner approval policy. A denial is not permission to route around policy.

Only the target owner changes grants: `messh grant ls`, `messh grant add KIND --actions ACTIONS --agent AGENT --device DEVICE_ID --expires-in DURATION` (optional `--tool`, `--args-hash`, `--path`), or `messh grant rm ID`. Grants bind device ID + agent label, exact tool request or file subtree/action, and finite expiry. Revocation/expiry blocks new admissions; accepted work continues. File access is deny-by-default: pairing alone grants none; there is no paired-peer migration. Narrow grants cover listings, reads/writes, copies, and relays; artifacts cannot be written.

## Owner-published job recipes
Discover with `recipe_list`, then call `recipe_get` using the exact name, version, and digest it returned. Read its typed parameter schema and fixed execution details; do not infer or alter arguments. Example definition for owner publication (`messh recipe publish recipe.json`):

```json
{"name":"count","version":"1.0.0","command":"printf","args":[{"value":"%d\n"},{"parameter":"count"}],"parameters":{"type":"object","properties":{"count":{"type":"integer","minimum":1,"maximum":4}},"required":["count"],"additionalProperties":false}}
```
The example requires an installed `printf` executable on the target (for example, a Linux node). On other targets, the owner publishes a recipe for an appropriate installed executable; publishing a definition does not install that program.


After publication, use the digest actually returned by `recipe_get` (never copy a guessed or stale digest). For example, once discovery returns the current digest, submit with `{"request_id":"count-001","recipe":{"name":"count","version":"1.0.0","digest":"<digest returned by recipe_get>","parameters":{"count":3}}}`. The parameter is validated and occupies its own argv slot; the recipe fixes the command and execution settings. Recipes are immutable per version. Disable via owner-only `messh recipe disable NAME VERSION`; this blocks future submissions but leaves accepted job snapshots unchanged. Retrying uses the same `request_id` and exact payload; durable origin outbox/target receipts preserve accepted submissions across restart. A normal running process is not replayed after its node restarts.

Never publish or disable recipes as an agent without the owner’s exact instruction.

## Durable job events
Discover `job_events` via `mesh_tools`; it accepts `cursor`, `limit` (1..128), and `wait_ms` (0..30000). Store and resume the opaque `next_cursor`; it is owner/credential and source scoped, not a global sequence. On `cursor_expired`, reconcile the included snapshot against `job_list`/`job_status`; use the returned composite `next_cursor` when present. Target cursor expiration itself includes the target snapshot; the origin wrapper preserves its origin cursor while resetting only the target cursor. Offline target responses may be `stale:true`; this is not live target confirmation. Origin and target event streams are source-separated; do not infer a total order across them.

## Optional negotiated MCP Tasks (2026 extension)

The gateway advertises support for `io.modelcontextprotocol/tasks`. To request Task results, include this metadata on each tool call and Tasks method request:

```json
{
  "_meta": {
    "io.modelcontextprotocol/clientCapabilities": {
      "extensions": { "io.modelcontextprotocol/tasks": {} }
    }
  }
}
```

Negotiated full or compact `job_submit` calls return the Task/tool-result union (`resultType: "task"` on acceptance). Clients without the extension keep ordinary native tool-call results. Supported methods are `tasks/get` (`{taskId}`, with an inline result), `tasks/cancel` (`{taskId}`), and `tasks/update` (`{taskId,inputResponses?}`). This server issues no client input requests, so updates cannot supply an owner approval. There is no `tasks/list` or `tasks/result`.

The Tasks view is an adapter over native jobs, not another durable job state machine. Native `job_status` remains authoritative. A Task can remain working while delivery is pending or cancellation is unconfirmed; do not report either as target acceptance or completed cancellation. The origin preserves the same task ID across restart for its durable submission. Cross-agent access to another owner’s task is rejected. Owner approval still applies: a Task operation cannot approve itself or bypass the target’s gate. For work that should survive a disconnect/restart, retain the native `request_id` and reconcile through `job_status`/`job_events`.

Task handles and creation timestamps use durable admission metadata: native receipts include `submitted`, and remote-origin receipts additionally include `origin_submitted`. Replays and origin restarts preserve the handle and creation time, including after target delivery. Older outbox entries without admission metadata use their earliest retained origin-event timestamp or recorded ledger timestamp; this is the earliest known persisted time, not a reconstructed exact historical submission time.

The Go SDK v1.8.0 typed `CallTool` client contract cannot express this task/call union; clients must use union-aware response decoding when they negotiate Tasks. The mesh gateway negotiates and presents this per request, while native tool calls remain unchanged for clients that do not negotiate.

## Submit and follow

A job runs on the target as its logged-in user, in ws/<workspace>. Prefer an argument vector (one array item per argument), not a shell command. Accepted inputs are copied into the workspace and verified against their approved content before launch. The tool returns a job_id; use it for every later operation. Ask job_status for current state, reason, outputs, and recent logs; job_wait blocks up to 300 seconds (default 30), then returns status even if the job remains nonterminal. Use job_logs to read stdout/stderr or follow one stream by byte offset; job_list lists this agent's jobs on that target, optionally filtered by state.

Native CLI example (replace desktop with a handle/name from mesh_nodes; pass JSON as the second positional argument):

```sh
messh call desktop__job_submit '{"request_id":"report-2026-10-02","command":"python3","args":["-u","/usr/local/bin/report.py","--date","2026-10-02"],"timeout_seconds":3600}' --json
messh call desktop__job_status '{"job_id":"<job_id from submit>"}' --json
messh call desktop__job_wait '{"job_id":"<job_id from submit>","timeout_seconds":300}' --json
messh call desktop__job_logs '{"job_id":"<job_id from submit>","stream":"stdout","offset":0,"max_bytes":65536}' --json
messh call desktop__job_list '{}' --json
```

The angle-bracket job id above is a value to substitute from the submit response, not literal JSON input. For a running job, cancel only when requested: job_cancel withdraws pending approval, removes queued work, or terminates a running process tree. Delete only a finished job after retrieving needed outputs: job_delete removes its record and logs, and its workspace unless another job shares it. Deletion does not free a request key for reuse.

## Safe retry keys and delivery

A submit is accepted only after its durable job record is saved on the target. The caller (origin) also writes a durable outbox record before acknowledging a remote submit. For a caller-supplied request_id, the same key + same payload for the same authenticated agent and device resolves to the same stable job_id; a different payload with that key is a conflict. Receipts and deletion tombstones are retained, so a deleted job cannot be accidentally recreated by replay. If the request id is omitted, the client generates one, but a caller that may retry after a lost response should supply and persist its own key so it can repeat exactly the same submission.

A remote result may say pending_delivery: the origin saved the request, but the target has not yet confirmed receipt. Keep that key and inspect status/list again; do not create a second request. cancellation_pending means the cancellation was saved but the target has not confirmed the process stopped. An unreachable target can yield a cached response marked stale; it is the last known state, not live confirmation. Logs are not cached when the target is unreachable.

After an origin restart, repeating a still-pending submission returns the same job ID, workspace, and `pending_delivery` state until the target receipt is confirmed; an absent cached receipt is not a successful acceptance.

Use the same stable-key principle for scheduled job occurrences: every occurrence needs its own durable operation identity (the scheduler assigns a distinct request id per occurrence). A scheduled job submit can safely be delivered again with that occurrence key. Do not blindly replay an ambiguous non-job external tool call: it may have executed before its response was lost and may not be idempotent.

## Restart behavior and boundaries

Both the origin and target nodes need writable, persistent state directories with enough free disk space and permissions for their own node processes. The origin stores remote outbox requests and last-known status; the target stores accepted job records/receipts, workspaces, logs, and (when configured) checkpoint snapshots. Preserve each node's own state directory across restart, on the same node identity. Losing origin state loses its pending delivery/reconciliation record; losing target state loses the job receipt and any job/workspace/log/checkpoint data there. Neither node can reconstruct state that was deleted or lost. Keep copies of required input and output data independently if they must survive state-directory loss.

Queued and approval-waiting jobs are persisted. After restart, queued jobs that had already been approved resume scheduling; an approval that had not been durably committed is not presumed granted, and the job requires a fresh owner approval. A running ordinary command is stopped when its node shuts down or is interrupted by restart; it is not replayed automatically. Native jobs persist the accepted command, input snapshots, queue position/state, and results; that does not make arbitrary process memory durable.

### Optional application checkpoint and resume

Checkpointing is opt-in per submission, for a cooperative application only. Set recovery to an object with:

- checkpoint: a workspace-relative path for a regular checkpoint file (no path escape).
- args: the complete replacement argv for the same executable on resume—not an argv suffix. Jobs using a shell are not eligible.

The process receives MESSH_CHECKPOINT as the same absolute workspace path on its first attempt and on resume. Before a resumed launch, messh restores the last committed snapshot to that path and sets MESSH_RESUMED=1. messh snapshots a changed checkpoint at approximately two-second intervals and when the node shuts down gracefully; a snapshot is usable only after its durable commit. The checkpoint limit is 64 MiB. A hard crash can lose changes since the last committed snapshot; a missing, invalid, oversized, or damaged checkpoint is not recoverable. Resume keeps the original job id, original command/argv in its record, log cursor, and original timeout budget; only the replacement argv launches the same program with the checkpoint.

The application must make checkpoint publication atomic (write a temporary file, flush it as appropriate, then atomically replace the checkpoint) and ensure its checkpoint is self-consistent. messh can resume from the last committed checkpoint, not restore arbitrary OS/process memory. It does not guarantee exactly-once external effects: the application must make effects idempotent or record its own durable progress so a restart after the checkpoint does not duplicate work. A small runnable progress-counter example is:

```python
import json, os, tempfile

checkpoint = os.environ["MESSH_CHECKPOINT"]
resumed = os.environ.get("MESSH_RESUMED") == "1"
state = {"next": 0}
if resumed:
    with open(checkpoint, encoding="utf-8") as f:
        state = json.load(f)

for item in range(state["next"], 100):
    print(f"processed {item}", flush=True)
    state = {"next": item + 1}
    directory = os.path.dirname(checkpoint)
    fd, temporary = tempfile.mkstemp(prefix=".checkpoint-", dir=directory)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(state, f)
            f.flush()
            os.fsync(f.fileno())
        os.replace(temporary, checkpoint)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
```

This sample demonstrates an atomic progress file, not exactly-once side effects: real work must be idempotent or transactional with its saved progress. Submit it with the original and complete replacement argv, for example:

```json
{
  "command": "python3",
  "args": ["-u", "checkpointed.py"],
  "inputs": ["ws/source/checkpointed.py"],
  "recovery": {
    "checkpoint": "progress.json",
    "args": ["-u", "checkpointed.py", "--resume"]
  },
  "request_id": "checkpointed-report-1"
}
```

Before submitting, publish or copy the script to `ws/source/checkpointed.py` on the target. The `inputs` entry stages it as `checkpointed.py` in the newly created job workspace; that job workspace need not already exist. Use the same executable and workspace-relative script path in both argv arrays; the resumed application detects MESSH_RESUMED and reads its checkpoint. This mechanism is for application-level continuation, not a promise that all running commands will restart.