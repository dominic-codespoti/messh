// Run: node --test lib.test.ts   (Node >= 22.18 / 23.6 strips the types itself)

import assert from "node:assert/strict";
import * as http from "node:http";
import type { AddressInfo } from "node:net";
import { join } from "node:path";
import { after, before, describe, test } from "node:test";
import {
	adaptSchema,
	bearerFromOutput,
	bearerFromToken,
	configFilePath,
	DEFAULT_URL,
	mapResult,
	McpClient,
	MesshError,
	parseFileConfig,
	planTools,
	resolveConfig,
	tokenArgs,
	truncateHead,
} from "./lib.ts";

describe("config", () => {
	test("environment beats messh.json beats defaults", () => {
		const file = parseFileConfig('{"agent":"filepi","url":"http://127.0.0.1:7530/mcp","messh":"/opt/messh","state":"/srv/s"}', "f");
		const fromFile = resolveConfig({}, file, "f");
		assert.equal(fromFile.agent, "filepi");
		assert.equal(fromFile.url, "http://127.0.0.1:7530/mcp");
		assert.equal(fromFile.bin, "/opt/messh");
		assert.equal(fromFile.state, "/srv/s");

		const env = resolveConfig({ MESSH_AGENT: "envpi", MESSH_URL: "http://127.0.0.1:7540/mcp", MESSH_BIN: "C:\\messh\\messh.exe", MESSH_STATE: "D:\\st" }, file, "f");
		assert.deepEqual([env.agent, env.url, env.bin, env.state], ["envpi", "http://127.0.0.1:7540/mcp", "C:\\messh\\messh.exe", "D:\\st"]);

		const defaults = resolveConfig({ MESSH_AGENT: "  " }, {}, "f");
		assert.deepEqual([defaults.agent, defaults.url, defaults.bin, defaults.state, defaults.token], ["pi", DEFAULT_URL, "messh", undefined, undefined]);
	});

	test("bad config is reported, not ignored", () => {
		assert.throws(() => parseFileConfig("{nope", "f"), (e: unknown) => e instanceof MesshError && e.kind === "config");
		assert.throws(() => parseFileConfig('{"agent":3}', "f"), /"agent" must be a string/);
		assert.throws(() => resolveConfig({ MESSH_URL: "ftp://x/mcp" }, {}, "f"), /http or https/);
		assert.throws(() => resolveConfig({ MESSH_URL: "not a url" }, {}, "f"), /not a valid URL/);
	});

	test("config file lives in pi's agent dir", () => {
		assert.equal(configFilePath({}, "/home/u"), join("/home/u", ".pi", "agent", "messh.json"));
		assert.equal(configFilePath({ PI_CODING_AGENT_DIR: "~/alt" }, "/home/u"), join("/home/u", "alt", "messh.json"));
	});

	test("token command arguments carry --state only when set", () => {
		const base = resolveConfig({}, {}, "f");
		assert.deepEqual(tokenArgs(base), ["agent", "token", "pi", "--bearer"]);
		const withState = resolveConfig({ MESSH_AGENT: "raspi", MESSH_STATE: "C:\\Users\\Example\\my state" }, {}, "f");
		assert.deepEqual(tokenArgs(withState), ["agent", "token", "raspi", "--bearer", "--state", "C:\\Users\\Example\\my state"]);
	});

	test("bearer header from command output or literal token, never echoing it", () => {
		assert.equal(bearerFromOutput("Bearer abc123\r\n"), "Bearer abc123");
		assert.equal(bearerFromOutput("\nabc123\n"), "Bearer abc123");
		assert.equal(bearerFromToken("abc123"), "Bearer abc123");
		assert.equal(bearerFromToken("Bearer abc123"), "Bearer abc123");
		assert.throws(() => bearerFromOutput(""), /printed nothing/);
		try {
			bearerFromOutput("secret-ish words here");
			assert.fail("expected an error");
		} catch (e) {
			assert.ok(e instanceof MesshError && e.kind === "token");
			assert.ok(!e.message.includes("secret-ish"), "error message must not contain the command output");
		}
	});
});

describe("tool planning", () => {
	test("built-in collisions, invalid names and duplicates are skipped; vanished tools reported", () => {
		const plan = planTools(
			[{ name: "mesh_call" }, { name: "read" }, { name: "messh_refresh" }, { name: "bad name" }, { name: "desktop__node_info" }, { name: "mesh_call" }],
			new Set(["mesh_call", "laptop__node_info"]),
		);
		assert.deepEqual(plan.register.map((t) => t.name), ["mesh_call", "desktop__node_info"]);
		assert.deepEqual(plan.skipped.map((s) => s.name), ["read", "messh_refresh", "bad name", "mesh_call"]);
		assert.deepEqual(plan.removed, ["laptop__node_info"]);
	});

	test("schemas become objects with properties and inlined local refs", () => {
		assert.deepEqual(adaptSchema(undefined), { type: "object", properties: {} });
		assert.deepEqual(adaptSchema({ type: "object" }), { type: "object", properties: {} });
		const s = adaptSchema({
			$schema: "https://json-schema.org/draft/2020-12/schema",
			type: "object",
			properties: { voice: { $ref: "#/$defs/Voice", description: "which voice" }, node: { $ref: "#/$defs/Node" } },
			required: ["voice"],
			$defs: {
				Voice: { type: "string", enum: ["a", "b"] },
				Node: { type: "object", properties: { child: { $ref: "#/$defs/Node" } } },
			},
		});
		assert.deepEqual(s, {
			type: "object",
			properties: {
				voice: { type: "string", enum: ["a", "b"], description: "which voice" },
				node: { type: "object", properties: { child: {} } },
			},
			required: ["voice"],
		});
	});
});

describe("results", () => {
	test("MCP content maps to pi text and image content", () => {
		const r = mapResult({
			content: [
				{ type: "text", text: "hello" },
				{ type: "image", data: "aGVsbG8=", mimeType: "image/png" },
				{ type: "audio", data: "aGVsbG8=", mimeType: "audio/wav" },
				{ type: "resource_link", uri: "file:///x.wav", name: "x.wav", mimeType: "audio/wav", size: 5 },
				{ type: "resource", resource: { uri: "mem://a", text: "inline" } },
				{ type: "resource", resource: { uri: "mem://b", mimeType: "application/zip", blob: "aGk=" } },
			],
		});
		assert.equal(r.isError, false);
		assert.deepEqual(r.images, [{ type: "image", data: "aGVsbG8=", mimeType: "image/png" }]);
		assert.equal(
			r.text,
			[
				"hello",
				"[audio content: audio/wav, 5 bytes; not shown]",
				"[resource link] x.wav file:///x.wav (audio/wav, 5 bytes)",
				"[resource mem://a]\ninline",
				"[resource mem://b: application/zip, 2 bytes; not shown]",
			].join("\n\n"),
		);
	});

	test("structured content is shown when there is no other content; errors flagged", () => {
		const r = mapResult({ content: [], structuredContent: { ok: 1 }, isError: true });
		assert.equal(r.isError, true);
		assert.equal(r.text, '{\n  "ok": 1\n}');
	});

	test("long output keeps its head within pi's limits", () => {
		const lines = Array.from({ length: 3000 }, (_, i) => `line ${i}`).join("\n");
		const byLines = truncateHead(lines);
		assert.equal(byLines.truncated, true);
		assert.equal(byLines.text.split("\n").length, 2000);
		assert.equal(byLines.totalLines, 3000);

		const wide = "é".repeat(40_000); // 80 000 bytes on one line
		const byBytes = truncateHead(wide);
		assert.equal(byBytes.truncated, true);
		assert.ok(Buffer.byteLength(byBytes.text, "utf8") <= 50 * 1024);
		assert.ok(!byBytes.text.includes("\uFFFD"), "no broken character at the cut");

		assert.equal(truncateHead("short").truncated, false);
	});
});

describe("MCP client against a stateless JSON endpoint", () => {
	let server: http.Server;
	let url: string;
	const seen: { method: string; accept?: string; protocol?: string }[] = [];
	// Resolved when the server receives a "slow" call, so the test aborts it mid-flight.
	const slowArrivals: (() => void)[] = [];

	before(async () => {
		server = http.createServer((req, res) => {
			let body = "";
			req.on("data", (c) => (body += c));
			req.on("end", () => {
				if (req.headers.authorization !== "Bearer good") {
					res.writeHead(401, { "WWW-Authenticate": "Bearer" }).end("invalid token");
					return;
				}
				const msg = JSON.parse(body);
				seen.push({ method: msg.method, accept: req.headers.accept, protocol: req.headers["mcp-protocol-version"] as string });
				const reply = (result: unknown) => {
					res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify({ jsonrpc: "2.0", id: msg.id, result }));
				};
				if (msg.method === "tools/list" && !msg.params.cursor) {
					reply({ tools: [{ name: "mesh_nodes", inputSchema: { type: "object" } }], nextCursor: "p2" });
				} else if (msg.method === "tools/list") {
					reply({ tools: [{ name: "desktop__node_info", description: "info" }] });
				} else if (msg.params.name === "slow") {
					// never answers; the client must give up
					slowArrivals.shift()?.();
				} else if (msg.params.name === "sse") {
					res.writeHead(200, { "Content-Type": "text/event-stream" });
					res.end(`event: message\ndata: ${JSON.stringify({ jsonrpc: "2.0", id: msg.id, result: { content: [{ type: "text", text: "via sse" }] } })}\n\n`);
				} else if (msg.params.name === "missing") {
					res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify({ jsonrpc: "2.0", id: msg.id, error: { code: -32602, message: "unknown tool \"missing\"" } }));
				} else {
					reply({ content: [{ type: "text", text: `args=${JSON.stringify(msg.params.arguments)}` }], isError: msg.params.name === "fails" });
				}
			});
		});
		await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
		url = `http://127.0.0.1:${(server.address() as AddressInfo).port}/mcp`;
	});

	after(() => {
		server.closeAllConnections();
		server.close();
	});

	test("tools/list follows cursors without initialize and sends the streamable HTTP headers", async () => {
		const tools = await new McpClient(url, "Bearer good").listTools();
		assert.deepEqual(tools.map((t) => t.name), ["mesh_nodes", "desktop__node_info"]);
		assert.ok(seen.every((s) => s.method !== "initialize"));
		assert.equal(seen[0].accept, "application/json, text/event-stream");
		assert.equal(seen[0].protocol, "2025-11-25");
	});

	test("tools/call returns content and the error flag", async () => {
		const c = new McpClient(url, "Bearer good");
		const ok = await c.callTool("desktop__node_info", { a: 1 }, undefined);
		assert.deepEqual(ok, { content: [{ type: "text", text: 'args={"a":1}' }], structuredContent: undefined, isError: false });
		assert.equal((await c.callTool("fails", {}, undefined)).isError, true);
		assert.deepEqual((await c.callTool("sse", {}, undefined)).content, [{ type: "text", text: "via sse" }]);
	});

	test("failures are classified for the fix-it advice", async () => {
		const kind = async (p: Promise<unknown>) => {
			try {
				await p;
			} catch (e) {
				return e instanceof MesshError ? e.kind : String(e);
			}
			return "no error";
		};
		assert.equal(await kind(new McpClient(url, "Bearer wrong").listTools()), "unauthorized");
		assert.equal(await kind(new McpClient(url, "Bearer good").callTool("missing", {}, undefined)), "rpc");
		// The client's own timeout is a real AbortSignal.timeout; keep it tiny.
		assert.equal(await kind(new McpClient(url, "Bearer good").callTool("slow", {}, undefined, 20)), "timeout");
		const ac = new AbortController();
		const arrived = new Promise<void>((resolve) => slowArrivals.push(resolve));
		const cancelled = kind(new McpClient(url, "Bearer good").callTool("slow", {}, ac.signal));
		await arrived;
		ac.abort();
		assert.equal(await cancelled, "cancelled");

		const closed = http.createServer();
		await new Promise<void>((resolve) => closed.listen(0, "127.0.0.1", resolve));
		const deadURL = `http://127.0.0.1:${(closed.address() as AddressInfo).port}/mcp`;
		await new Promise<void>((resolve) => closed.close(() => resolve()));
		assert.equal(await kind(new McpClient(deadURL, "Bearer good").listTools()), "unreachable");
	});
});
