// messh pi extension: publishes the tools of the local messh node (other LAN
// devices' GPUs, services, browsers, files and jobs) as native pi tools.
// Only type imports from pi, so it loads in pi and in harnesses that load pi
// extensions (omp) without installing anything.

import type { ExtensionAPI, ToolDefinition } from "@earendil-works/pi-coding-agent";
import type { TSchema } from "typebox";
import { execFile } from "node:child_process";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";
import {
	adaptSchema,
	advice,
	bearerFromOutput,
	bearerFromToken,
	type Config,
	configFilePath,
	errorText,
	type FileConfig,
	mapResult,
	type McpCallResult,
	McpClient,
	type McpTool,
	MesshError,
	type PiImage,
	type PiText,
	parseFileConfig,
	planTools,
	REFRESH_TOOL,
	resolveConfig,
	STATUS_TOOL,
	type ToolPlan,
	tokenArgs,
	toolDescription,
	truncateHead,
} from "./lib.ts";

const TOKEN_TIMEOUT_MS = 15_000;
const NO_PARAMS = { type: "object", properties: {}, additionalProperties: false } as TSchema;

interface Details {
	tool: string;
	fullOutput?: string;
}

interface SyncReport {
	tools: string[];
	added: string[];
	removed: string[];
	skipped: ToolPlan["skipped"];
}

export default async function messhExtension(pi: ExtensionAPI): Promise<void> {
	let config: Config | undefined;
	let client: McpClient | undefined;
	let problem: MesshError | undefined;
	let skipped: ToolPlan["skipped"] = [];
	// pi.getActiveTools/setActiveTools throw until the runtime is bound (before session_start).
	let bound = false;
	let notified = false;
	let statusRegistered = false;
	let refreshRegistered = false;
	// MCP tools messh offers now; registered pi tools cannot be unregistered, so
	// tools messh stopped offering are deactivated and remembered in `retired`.
	let offered = new Set<string>();
	const retired = new Set<string>();

	async function loadConfig(): Promise<Config> {
		const path = configFilePath(process.env, homedir());
		let file: FileConfig = {};
		try {
			file = parseFileConfig(await readFile(path, "utf8"), path);
		} catch (err) {
			if (err instanceof MesshError) throw err;
			if ((err as NodeJS.ErrnoException).code !== "ENOENT") {
				throw new MesshError("config", `cannot read ${path}: ${errorText(err)}`);
			}
		}
		return resolveConfig(process.env, file, path);
	}

	// No shell: MESSH_BIN and the state path are passed as plain arguments.
	function runTokenCommand(cfg: Config, signal: AbortSignal | undefined): Promise<string> {
		const args = tokenArgs(cfg);
		const shown = `${cfg.bin} ${args.join(" ")}`;
		// Executor form, not Promise.withResolvers: pi supports Node 20, which lacks it.
		return new Promise((resolve, reject) => {
			execFile(
				cfg.bin,
				args,
				{ timeout: TOKEN_TIMEOUT_MS, windowsHide: true, maxBuffer: 64 * 1024, encoding: "utf8", signal },
				(err, stdout, stderr) => {
					if (err) {
						let why: string;
						if (err.code === "ENOENT") why = `${cfg.bin} was not found`;
						else if (err.name === "AbortError") why = "cancelled";
						else if (err.killed) why = `it did not finish within ${TOKEN_TIMEOUT_MS / 1000} s`;
						// stderr carries messh's own error ("agent ... is not registered"); stdout is never shown.
						else why = stderr.trim().split(/\r?\n/)[0] || `exit code ${String(err.code)}`;
						reject(new MesshError("token", `\`${shown}\` failed: ${why}`));
						return;
					}
					try {
						resolve(bearerFromOutput(stdout));
					} catch (parseErr) {
						reject(parseErr);
					}
				},
			);
		});
	}

	async function connect(signal: AbortSignal | undefined): Promise<McpClient> {
		config = await loadConfig();
		const auth = config.token ? bearerFromToken(config.token) : await runTokenCommand(config, signal);
		return new McpClient(config.url, auth);
	}

	function problemText(err: MesshError): string {
		return `messh tools are unavailable: ${err.message}\n${advice(err, config)}`;
	}

	function asMesshError(err: unknown): MesshError {
		return err instanceof MesshError ? err : new MesshError("protocol", errorText(err));
	}

	async function sync(signal: AbortSignal | undefined): Promise<SyncReport> {
		const next = await connect(signal);
		const list = await next.listTools(signal);
		client = next;
		problem = undefined;
		const plan = planTools(list, offered);
		const added = plan.register.filter((t) => !offered.has(t.name)).map((t) => t.name);
		offered = new Set(plan.register.map((t) => t.name));
		skipped = plan.skipped;
		// Re-registering replaces the definition, so changed descriptions and schemas apply too.
		for (const t of plan.register) pi.registerTool(piTool(t));
		registerRefreshTool();
		if (bound) {
			const active = pi.getActiveTools();
			const nextActive = active.filter((n) => n !== STATUS_TOOL && !plan.removed.includes(n));
			for (const n of plan.removed) retired.add(n);
			for (const t of plan.register) {
				if (retired.delete(t.name) && !nextActive.includes(t.name)) nextActive.push(t.name);
			}
			if (nextActive.length !== active.length || nextActive.some((n, i) => n !== active[i])) {
				pi.setActiveTools(nextActive);
			}
		}
		return { tools: [...offered], added, removed: plan.removed, skipped: plan.skipped };
	}

	async function syncOrExplain(signal: AbortSignal | undefined): Promise<SyncReport> {
		try {
			return await sync(signal);
		} catch (err) {
			problem = asMesshError(err);
			throw new Error(problemText(problem));
		}
	}

	function summary(r: SyncReport): string {
		const lines = [
			`Connected to messh at ${config?.url} as agent "${config?.agent}".`,
			`${r.tools.length} messh tools: ${r.tools.join(", ") || "(none)"}`,
		];
		if (r.added.length > 0) lines.push(`added: ${r.added.join(", ")}`);
		if (r.removed.length > 0) lines.push(`no longer offered (deactivated): ${r.removed.join(", ")}`);
		if (r.skipped.length > 0) lines.push(`skipped: ${r.skipped.map((s) => `${s.name} (${s.reason})`).join(", ")}`);
		return lines.join("\n");
	}

	async function call(name: string, params: unknown, signal: AbortSignal | undefined): Promise<McpCallResult> {
		try {
			if (!client) client = await connect(signal);
			return await client.callTool(name, params, signal);
		} catch (err) {
			const e = asMesshError(err);
			if (e.kind !== "unauthorized") throw new Error(e.kind === "cancelled" ? e.message : `${e.message}\n${advice(e, config)}`);
		}
		// HTTP 401 means nothing ran; the token may have been re-issued
		// (`messh agent rm` + `add`), so fetch it once more and retry.
		try {
			client = await connect(signal);
			return await client.callTool(name, params, signal);
		} catch (err) {
			const e = asMesshError(err);
			throw new Error(`${e.message}\n${advice(e, config)}`);
		}
	}

	async function toPiResult(name: string, toolCallId: string, r: McpCallResult) {
		const mapped = mapResult(r);
		const cut = truncateHead(mapped.text);
		if (mapped.isError) throw new Error(cut.text.trim() || `${name} failed without an error message`);
		const details: Details = { tool: name };
		let text = cut.text;
		if (cut.truncated) {
			text += `\n\n[messh: output truncated to ${Buffer.byteLength(cut.text, "utf8")} of ${cut.totalBytes} bytes, ${cut.totalLines} lines in total.`;
			try {
				const dir = join(tmpdir(), "messh-pi");
				await mkdir(dir, { recursive: true });
				const file = join(dir, `${name}-${Date.now()}-${toolCallId.replace(/[^A-Za-z0-9_-]/g, "")}.txt`);
				await writeFile(file, mapped.text, "utf8");
				details.fullOutput = file;
				text += ` Full output: ${file}]`;
			} catch {
				text += "]";
			}
		}
		const content: (PiText | PiImage)[] = [];
		if (text !== "" || mapped.images.length === 0) content.push({ type: "text", text: text || "(no output)" });
		content.push(...mapped.images);
		return { content, details };
	}

	function piTool(t: McpTool): ToolDefinition<TSchema, Details> {
		return {
			name: t.name,
			label: t.title || t.annotations?.title || t.name,
			description: toolDescription(t),
			parameters: adaptSchema(t.inputSchema) as TSchema,
			async execute(toolCallId, params, signal) {
				if (!offered.has(t.name)) {
					throw new Error(`${t.name} is no longer offered by messh. Call ${REFRESH_TOOL} or mesh_nodes to see the current tools.`);
				}
				return toPiResult(t.name, toolCallId, await call(t.name, params, signal));
			},
		};
	}

	function registerRefreshTool(): void {
		if (refreshRegistered) return;
		refreshRegistered = true;
		pi.registerTool<TSchema, Details>({
			name: REFRESH_TOOL,
			label: "messh refresh",
			description:
				"[messh] Reload the list of messh tools from the local messh node, e.g. after a device was paired or a service added, " +
				"or when a messh tool is reported missing. Returns the current tools and what was added or removed.",
			parameters: NO_PARAMS,
			async execute(_toolCallId, _params, signal) {
				const report = await syncOrExplain(signal);
				return { content: [{ type: "text", text: summary(report) }], details: { tool: REFRESH_TOOL } };
			},
		});
	}

	function registerStatusTool(): void {
		if (statusRegistered) return;
		statusRegistered = true;
		pi.registerTool<TSchema, Details>({
			name: STATUS_TOOL,
			label: "messh status",
			description:
				"[messh] The messh tools (other devices on this LAN: their GPUs, local AI services, browsers, files and jobs) could not be loaded. " +
				"Call this to see why and how to fix it; it also retries connecting, and on success the messh tools become available.",
			parameters: NO_PARAMS,
			async execute(_toolCallId, _params, signal) {
				const report = await syncOrExplain(signal);
				return { content: [{ type: "text", text: summary(report) }], details: { tool: STATUS_TOOL } };
			},
		});
	}

	pi.registerCommand("messh", {
		description: "Connect to the local messh node again and reload its tools",
		handler: async (_args, ctx) => {
			try {
				ctx.ui.notify(summary(await sync(ctx.signal)), "info");
			} catch (err) {
				problem = asMesshError(err);
				registerStatusTool();
				ctx.ui.notify(problemText(problem), "error");
			}
		},
	});

	pi.on("session_start", (_event, ctx) => {
		bound = true;
		if (notified || !ctx.hasUI) return;
		if (problem) {
			notified = true;
			ctx.ui.notify(`${problemText(problem)}\nRun /messh to retry.`, "warning");
		} else if (skipped.length > 0) {
			notified = true;
			ctx.ui.notify(`messh skipped tools: ${skipped.map((s) => `${s.name} (${s.reason})`).join(", ")}`, "warning");
		}
	});

	try {
		await sync(undefined);
	} catch (err) {
		problem = asMesshError(err);
		registerStatusTool();
	}
}
