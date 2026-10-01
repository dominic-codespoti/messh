// Pure helpers and the MCP client of the messh pi extension. Kept free of pi
// runtime imports so `node --test` can exercise them without pi installed.

import * as http from "node:http";
import * as https from "node:https";
import { join } from "node:path";

export const DEFAULT_URL = "http://127.0.0.1:7520/mcp";
export const DEFAULT_AGENT = "pi";
export const DEFAULT_BIN = "messh";
// Old-protocol version: the Go SDK's stateless handler synthesizes the
// initialize state for these, so tools/list and tools/call work without an
// initialize round trip (mcp/streamable.go, ephemeralConnectOpts).
export const PROTOCOL_VERSION = "2025-11-25";
export const LIST_TIMEOUT_MS = 15_000;
// job_wait may block for up to 300 s; leave headroom for the mesh round trip.
export const CALL_TIMEOUT_MS = 330_000;
export const MAX_RESPONSE_BYTES = 64 * 1024 * 1024;
// pi's own tool output limits (DEFAULT_MAX_BYTES / DEFAULT_MAX_LINES).
export const MAX_OUTPUT_BYTES = 50 * 1024;
export const MAX_OUTPUT_LINES = 2000;

export const STATUS_TOOL = "messh_status";
export const REFRESH_TOOL = "messh_refresh";
// pi's built-in tools (core/tools/index.js allToolNames) plus this extension's own.
export const RESERVED_TOOL_NAMES: Readonly<Record<string, true>> = {
	read: true,
	bash: true,
	edit: true,
	write: true,
	grep: true,
	find: true,
	ls: true,
	[STATUS_TOOL]: true,
	[REFRESH_TOOL]: true,
};
export const TOOL_NAME_RE = /^[A-Za-z0-9_-]{1,64}$/;

export type ProblemKind =
	| "config"
	| "token"
	| "unreachable"
	| "unauthorized"
	| "timeout"
	| "cancelled"
	| "protocol"
	| "rpc";

export class MesshError extends Error {
	kind: ProblemKind;
	constructor(kind: ProblemKind, message: string) {
		super(message);
		this.name = "MesshError";
		this.kind = kind;
	}
}

export function errorText(err: unknown): string {
	if (err instanceof Error) {
		const code = (err as NodeJS.ErrnoException).code;
		return code && !err.message.includes(code) ? `${err.message} (${code})` : err.message;
	}
	return String(err);
}

// ---------------------------------------------------------------- config

export interface FileConfig {
	agent?: string;
	url?: string;
	messh?: string;
	state?: string;
}

export interface Config {
	agent: string;
	url: string;
	bin: string;
	state?: string;
	token?: string;
	configFile: string;
}

export type Env = Record<string, string | undefined>;

/** Location of the optional messh.json: pi's agent dir (PI_CODING_AGENT_DIR or ~/.pi/agent). */
export function configFilePath(env: Env, home: string): string {
	let dir = env.PI_CODING_AGENT_DIR?.trim();
	if (dir) {
		if (dir === "~") dir = home;
		else if (dir.startsWith("~/") || dir.startsWith("~\\")) dir = join(home, dir.slice(2));
	} else {
		dir = join(home, ".pi", "agent");
	}
	return join(dir, "messh.json");
}

export function parseFileConfig(text: string, path: string): FileConfig {
	let raw: unknown;
	try {
		raw = JSON.parse(text);
	} catch (err) {
		throw new MesshError("config", `${path} is not valid JSON: ${errorText(err)}`);
	}
	if (!isRecord(raw)) throw new MesshError("config", `${path} must contain a JSON object`);
	const out: FileConfig = {};
	for (const key of ["agent", "url", "messh", "state"] as const) {
		const v = raw[key];
		if (v === undefined || v === null) continue;
		if (typeof v !== "string") throw new MesshError("config", `${path}: "${key}" must be a string`);
		if (v.trim() !== "") out[key] = v.trim();
	}
	return out;
}

/** Environment variables win over messh.json, which wins over the defaults. */
export function resolveConfig(env: Env, file: FileConfig, configFile: string): Config {
	const pick = (v: string | undefined) => (v && v.trim() !== "" ? v.trim() : undefined);
	const cfg: Config = {
		agent: pick(env.MESSH_AGENT) ?? file.agent ?? DEFAULT_AGENT,
		url: pick(env.MESSH_URL) ?? file.url ?? DEFAULT_URL,
		bin: pick(env.MESSH_BIN) ?? file.messh ?? DEFAULT_BIN,
		configFile,
	};
	const state = pick(env.MESSH_STATE) ?? file.state;
	if (state) cfg.state = state;
	const token = pick(env.MESSH_TOKEN);
	if (token) cfg.token = token;
	let protocol = "";
	try {
		protocol = new URL(cfg.url).protocol;
	} catch {
		throw new MesshError("config", `messh URL ${JSON.stringify(cfg.url)} is not a valid URL`);
	}
	if (protocol !== "http:" && protocol !== "https:") {
		throw new MesshError("config", `messh URL ${cfg.url} must use http or https`);
	}
	return cfg;
}

/** Arguments for `<bin> agent token <agent> --bearer [--state DIR]`, run without a shell. */
export function tokenArgs(cfg: Config): string[] {
	const args = ["agent", "token", cfg.agent, "--bearer"];
	if (cfg.state) args.push("--state", cfg.state);
	return args;
}

/** Authorization header value from a literal token (with or without the Bearer prefix). */
export function bearerFromToken(token: string): string {
	const t = token.trim().replace(/^bearer\s+/i, "");
	if (t === "" || /\s/.test(t)) throw new MesshError("token", "MESSH_TOKEN is not a single token");
	return `Bearer ${t}`;
}

/** Authorization header value from the stdout of `messh agent token NAME --bearer`. */
export function bearerFromOutput(stdout: string): string {
	const line = stdout.split(/\r?\n/).find((l) => l.trim() !== "");
	if (!line) throw new MesshError("token", "the token command printed nothing");
	const t = line.trim().replace(/^bearer\s+/i, "");
	// Never echo the output: it is (or contains) the secret.
	if (t === "" || /\s/.test(t)) throw new MesshError("token", "the token command printed something other than a token");
	return `Bearer ${t}`;
}

/** Fix-it text for a problem, shown by messh_status, /messh and failing tool calls. */
export function advice(err: MesshError, cfg: Config | undefined): string {
	const agent = cfg?.agent ?? DEFAULT_AGENT;
	const url = cfg?.url ?? DEFAULT_URL;
	const bin = cfg?.bin ?? DEFAULT_BIN;
	const state = cfg?.state ? ` --state ${/[\s"]/.test(cfg.state) ? JSON.stringify(cfg.state) : cfg.state}` : "";
	switch (err.kind) {
		case "config":
			return `Fix the setting (environment MESSH_AGENT/MESSH_URL/MESSH_BIN/MESSH_STATE/MESSH_TOKEN or ${cfg?.configFile ?? "~/.pi/agent/messh.json"}).`;
		case "token":
			return (
				`Check that \`${bin} agent token ${agent} --bearer${state}\` works in a terminal on this device. ` +
				`Register the agent with \`messh agent add ${agent}\` if needed, put messh on PATH or set MESSH_BIN to its full path, ` +
				`and set MESSH_AGENT/MESSH_STATE if this agent or state directory is not the default.`
			);
		case "unreachable":
			return (
				`Start the messh node on this device (\`messh node\`, or \`systemctl --user start messh\`). ` +
				`\`messh status\` shows the node's local address; if it is not ${DEFAULT_URL}, set MESSH_URL to http://<address>/mcp.`
			);
		case "unauthorized":
			return (
				`The node rejected the token of agent "${agent}". Run \`messh agent add ${agent}\` on this device, ` +
				`and make sure MESSH_STATE (and MESSH_URL) point at the same node that serves ${url}.`
			);
		case "timeout":
			return "The node is running but slow to answer; try again, or check `messh status`.";
		case "cancelled":
			return "The call was cancelled.";
		case "protocol":
			return `Check that MESSH_URL (${url}) is the agent endpoint of a messh node (it ends in /mcp).`;
		case "rpc":
			return `Call ${REFRESH_TOOL} (or /messh) to reload the current messh tools.`;
	}
}

// ---------------------------------------------------------------- MCP client

export interface HttpResponse {
	status: number;
	contentType: string;
	body: string;
}

export type Post = (url: string, headers: Record<string, string>, body: string, signal: AbortSignal) => Promise<HttpResponse>;

/**
 * POST with node:http. Not fetch: undici's default 300 s headers timeout would
 * cut off a long job_wait before messh answers.
 */
export const nodePost: Post = (url, headers, body, signal) =>
	// Executor form, not Promise.withResolvers: pi supports Node 20, which lacks it.
	new Promise<HttpResponse>((resolve, reject) => {
		const u = new URL(url);
		// https.request accepts everything http.request does; the cast only merges their overloads.
		const request = (u.protocol === "https:" ? https.request : http.request) as typeof http.request;
		const payload = Buffer.from(body, "utf8");
		const req = request(
			u,
			{ method: "POST", headers: { ...headers, "Content-Length": String(payload.length) }, signal },
			(res) => {
				const chunks: Buffer[] = [];
				let size = 0;
				res.on("data", (chunk: Buffer) => {
					size += chunk.length;
					if (size > MAX_RESPONSE_BYTES) {
						req.destroy(new MesshError("protocol", `messh response larger than ${MAX_RESPONSE_BYTES} bytes`));
						return;
					}
					chunks.push(chunk);
				});
				res.on("error", reject);
				res.on("end", () =>
					resolve({
						status: res.statusCode ?? 0,
						contentType: String(res.headers["content-type"] ?? ""),
						body: Buffer.concat(chunks).toString("utf8"),
					}),
				);
				res.on("close", () => {
					if (!res.complete) reject(new MesshError("unreachable", "connection to messh closed before the answer was complete"));
				});
			},
		);
		req.on("error", reject);
		req.end(payload);
	});

export interface McpTool {
	name: string;
	title?: string;
	description?: string;
	inputSchema?: unknown;
	annotations?: { title?: string; [key: string]: unknown };
}

export interface McpCallResult {
	content: unknown[];
	structuredContent?: unknown;
	isError?: boolean;
}

interface RpcMessage {
	id?: unknown;
	result?: unknown;
	error?: { code?: unknown; message?: unknown };
}

export class McpClient {
	url: string;
	auth: string;
	post: Post;
	nextId = 1;

	constructor(url: string, auth: string, post: Post = nodePost) {
		this.url = url;
		this.auth = auth;
		this.post = post;
	}

	async rpc(method: string, params: unknown, signal: AbortSignal | undefined, timeoutMs: number): Promise<unknown> {
		const id = this.nextId++;
		const timeout = AbortSignal.timeout(timeoutMs);
		const combined = signal ? AbortSignal.any([signal, timeout]) : timeout;
		const headers = {
			"Content-Type": "application/json",
			Accept: "application/json, text/event-stream",
			"MCP-Protocol-Version": PROTOCOL_VERSION,
			Authorization: this.auth,
		};
		let res: HttpResponse;
		try {
			res = await this.post(this.url, headers, JSON.stringify({ jsonrpc: "2.0", id, method, params }), combined);
		} catch (err) {
			if (signal?.aborted) throw new MesshError("cancelled", `${method} cancelled`);
			if (timeout.aborted) throw new MesshError("timeout", `messh did not answer ${method} within ${Math.round(timeoutMs / 1000)} s`);
			if (err instanceof MesshError) throw err;
			throw new MesshError("unreachable", `cannot reach messh at ${this.url}: ${errorText(err)}`);
		}
		if (res.status === 401 || res.status === 403) {
			throw new MesshError("unauthorized", `messh at ${this.url} rejected the agent token (HTTP ${res.status})`);
		}
		if (res.status < 200 || res.status >= 300) {
			throw new MesshError("protocol", `messh at ${this.url} answered HTTP ${res.status}: ${snippet(res.body)}`);
		}
		const msg = findResponse(res, id);
		if (msg.error) {
			const code = typeof msg.error.code === "number" ? ` (code ${msg.error.code})` : "";
			throw new MesshError("rpc", `messh ${method} failed: ${String(msg.error.message ?? "unknown error")}${code}`);
		}
		return msg.result;
	}

	async listTools(signal?: AbortSignal): Promise<McpTool[]> {
		const tools: McpTool[] = [];
		let cursor: string | undefined;
		for (let page = 0; page < 100; page++) {
			const r = await this.rpc("tools/list", cursor ? { cursor } : {}, signal, LIST_TIMEOUT_MS);
			if (!isRecord(r) || !Array.isArray(r.tools)) throw new MesshError("protocol", "tools/list returned no tool list");
			for (const t of r.tools) {
				if (isRecord(t) && typeof t.name === "string") tools.push(t as unknown as McpTool);
			}
			if (typeof r.nextCursor !== "string" || r.nextCursor === "") return tools;
			cursor = r.nextCursor;
		}
		throw new MesshError("protocol", "tools/list did not end after 100 pages");
	}

	async callTool(name: string, args: unknown, signal: AbortSignal | undefined, timeoutMs = CALL_TIMEOUT_MS): Promise<McpCallResult> {
		const r = await this.rpc("tools/call", { name, arguments: isRecord(args) ? args : {} }, signal, timeoutMs);
		if (!isRecord(r)) throw new MesshError("protocol", `tools/call ${name} returned no result`);
		return {
			content: Array.isArray(r.content) ? r.content : [],
			structuredContent: r.structuredContent,
			isError: r.isError === true,
		};
	}
}

function findResponse(res: HttpResponse, id: number): RpcMessage {
	const candidates: unknown[] = [];
	try {
		if (res.contentType.toLowerCase().includes("text/event-stream")) {
			for (const event of res.body.split(/\r?\n\r?\n/)) {
				const data = event
					.split(/\r?\n/)
					.filter((l) => l.startsWith("data:"))
					.map((l) => l.slice(5).replace(/^ /, ""))
					.join("\n");
				if (data !== "") candidates.push(JSON.parse(data));
			}
		} else {
			const parsed = JSON.parse(res.body);
			if (Array.isArray(parsed)) candidates.push(...parsed);
			else candidates.push(parsed);
		}
	} catch {
		throw new MesshError("protocol", `messh answered something that is not JSON-RPC: ${snippet(res.body)}`);
	}
	for (const c of candidates) {
		if (isRecord(c) && c.id === id && ("result" in c || "error" in c)) return c as RpcMessage;
	}
	throw new MesshError("protocol", `messh answered without a response to request ${id}: ${snippet(res.body)}`);
}

function snippet(s: string): string {
	const t = s.trim().replace(/\s+/g, " ");
	return t === "" ? "(empty body)" : t.length > 300 ? `${t.slice(0, 300)}…` : t;
}

// ---------------------------------------------------------------- tools

export interface ToolPlan {
	register: McpTool[];
	skipped: { name: string; reason: string }[];
	removed: string[];
}

/** Which MCP tools to (re-)register as pi tools, which to skip and which disappeared. */
export function planTools(list: McpTool[], current: ReadonlySet<string>): ToolPlan {
	const seen = new Set<string>();
	const plan: ToolPlan = { register: [], skipped: [], removed: [] };
	for (const t of list) {
		if (!TOOL_NAME_RE.test(t.name)) {
			plan.skipped.push({ name: t.name, reason: "not a valid pi tool name" });
		} else if (Object.hasOwn(RESERVED_TOOL_NAMES, t.name)) {
			plan.skipped.push({ name: t.name, reason: "collides with a pi built-in or messh extension tool" });
		} else if (seen.has(t.name)) {
			plan.skipped.push({ name: t.name, reason: "listed twice" });
		} else {
			seen.add(t.name);
			plan.register.push(t);
		}
	}
	plan.removed = [...current].filter((n) => !seen.has(n));
	return plan;
}

export function toolDescription(t: McpTool): string {
	const d = (t.description ?? "").trim();
	return d === "" ? `[messh] ${t.name}` : `[messh] ${d}`;
}

/**
 * The MCP input schema as pi tool parameters. pi validates plain JSON Schema
 * (pi-ai validateToolArguments compiles it with TypeBox), but some providers
 * (Anthropic) only forward `properties` and `required`, so local $refs are
 * inlined and the root is forced to an object with properties.
 */
export function adaptSchema(input: unknown): Record<string, unknown> {
	const root: Record<string, unknown> = isRecord(input) ? structuredClone(input) : {};
	let s: Record<string, unknown> = { ...root };
	delete s.$defs;
	delete s.definitions;
	delete s.$schema;
	if (JSON.stringify(s).includes('"$ref"')) s = inlineRefs(s, root, new Set()) as Record<string, unknown>;
	if (s.type === undefined) s.type = "object";
	if (!isRecord(s.properties)) s.properties = {};
	return s;
}

function inlineRefs(node: unknown, root: Record<string, unknown>, stack: Set<string>): unknown {
	if (Array.isArray(node)) return node.map((n) => inlineRefs(n, root, stack));
	if (!isRecord(node)) return node;
	const { $ref: ref, ...siblings } = node;
	const out: Record<string, unknown> = {};
	if (typeof ref === "string") {
		const target = ref.startsWith("#") ? resolvePointer(root, ref) : undefined;
		// Recursive or unresolvable references become "any value" rather than a dangling $ref.
		if (isRecord(target) && !stack.has(ref) && stack.size < 32) {
			stack.add(ref);
			Object.assign(out, inlineRefs(target, root, stack));
			stack.delete(ref);
		}
	}
	for (const [k, v] of Object.entries(siblings)) out[k] = inlineRefs(v, root, stack);
	return out;
}

function resolvePointer(root: unknown, ref: string): unknown {
	if (ref === "#") return root;
	if (!ref.startsWith("#/")) return undefined;
	let cur: unknown = root;
	for (const raw of ref.slice(2).split("/")) {
		let key: string;
		try {
			key = decodeURIComponent(raw).replace(/~1/g, "/").replace(/~0/g, "~");
		} catch {
			return undefined;
		}
		if (!isRecord(cur) && !Array.isArray(cur)) return undefined;
		cur = (cur as Record<string, unknown>)[key];
	}
	return cur;
}

// ---------------------------------------------------------------- results

export interface PiText {
	type: "text";
	text: string;
}

export interface PiImage {
	type: "image";
	data: string;
	mimeType: string;
}

export interface MappedResult {
	isError: boolean;
	/** All text parts joined, plus notes for content pi cannot show. */
	text: string;
	images: PiImage[];
}

/** MCP tool result content -> pi content: text stays text, images stay images, the rest becomes a text note. */
export function mapResult(r: McpCallResult): MappedResult {
	const texts: string[] = [];
	const images: PiImage[] = [];
	for (const c of r.content) {
		if (!isRecord(c)) continue;
		switch (c.type) {
			case "text":
				if (typeof c.text === "string") texts.push(c.text);
				break;
			case "image":
				if (typeof c.data === "string" && typeof c.mimeType === "string") {
					images.push({ type: "image", data: c.data, mimeType: c.mimeType });
				} else {
					texts.push("[image content without data]");
				}
				break;
			case "audio":
				texts.push(`[audio content: ${String(c.mimeType ?? "unknown type")}, ${base64Size(c.data)} bytes; not shown]`);
				break;
			case "resource_link": {
				const meta = [c.mimeType, typeof c.size === "number" ? `${c.size} bytes` : undefined].filter(Boolean).join(", ");
				let line = `[resource link] ${[c.name ?? c.title, c.uri].filter((v) => typeof v === "string" && v !== "").join(" ")}`;
				if (meta !== "") line += ` (${meta})`;
				if (typeof c.description === "string" && c.description !== "") line += ` - ${c.description}`;
				texts.push(line);
				break;
			}
			case "resource": {
				const res: Record<string, unknown> = isRecord(c.resource) ? c.resource : {};
				if (typeof res.text === "string") {
					texts.push(`[resource ${String(res.uri ?? "")}]\n${res.text}`);
				} else {
					texts.push(`[resource ${String(res.uri ?? "")}: ${String(res.mimeType ?? "binary")}, ${base64Size(res.blob)} bytes; not shown]`);
				}
				break;
			}
			default:
				texts.push(`[unsupported ${String(c.type)} content]`);
		}
	}
	if (texts.length === 0 && images.length === 0 && r.structuredContent !== undefined) {
		texts.push(JSON.stringify(r.structuredContent, null, 2));
	}
	return { isError: r.isError === true, text: texts.join("\n\n"), images };
}

function base64Size(data: unknown): number {
	if (typeof data !== "string") return 0;
	const pad = data.endsWith("==") ? 2 : data.endsWith("=") ? 1 : 0;
	return Math.max(0, Math.floor((data.length * 3) / 4) - pad);
}

export interface Truncated {
	text: string;
	truncated: boolean;
	totalLines: number;
	totalBytes: number;
}

/** Keep the head of text within pi's tool output limits. */
export function truncateHead(text: string, maxBytes = MAX_OUTPUT_BYTES, maxLines = MAX_OUTPUT_LINES): Truncated {
	const totalBytes = Buffer.byteLength(text, "utf8");
	const lines = text.split("\n");
	const totalLines = lines.length;
	if (totalBytes <= maxBytes && totalLines <= maxLines) return { text, truncated: false, totalLines, totalBytes };
	let out = totalLines > maxLines ? lines.slice(0, maxLines).join("\n") : text;
	if (Buffer.byteLength(out, "utf8") > maxBytes) {
		// Cutting bytes can split a multi-byte character; drop the broken tail.
		out = Buffer.from(out, "utf8").subarray(0, maxBytes).toString("utf8").replace(/\uFFFD+$/, "");
	}
	return { text: out, truncated: true, totalLines, totalBytes };
}

export function isRecord(v: unknown): v is Record<string, unknown> {
	return typeof v === "object" && v !== null && !Array.isArray(v);
}
