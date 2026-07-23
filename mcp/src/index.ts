#!/usr/bin/env node
// krill-mcp — the M2 gate sentence as software: "Claude Code deploys and
// iterates on a real app with one tool call."
//
// A thin MCP wrapper over krilld's admin API (the ROADMAP's language
// decision: TypeScript only here, for the first-class MCP SDK; everything
// real lives in the Go daemon). Tools:
//
//   deploy      directory → Docker build → microVM rootfs → running app → URL
//   logs        serial tail + structured runtime errors (the self-heal loop)
//   apps        list apps and lifecycle states
//   delete_app  remove an app entirely
//
// Config: KRILL_ADMIN (default http://127.0.0.1:9091) — krilld's admin API,
// loopback-only, so run this on the host or bring an SSH tunnel.

import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";
import * as tar from "tar";
import { existsSync, statSync } from "node:fs";
import { basename, resolve } from "node:path";

const ADMIN = process.env.KRILL_ADMIN ?? "http://127.0.0.1:9091";
const DEPLOY_TIMEOUT_MS = 15 * 60 * 1000;

interface GuestError {
  kind: string;
  message: string;
  detail?: string;
  hint?: string;
}

interface DeployResp {
  app: { name: string; state: string; last_wake_ms: number };
  url: string;
  created: boolean;
  ready: boolean;
  wake_error?: string;
  errors?: GuestError[];
  size_mb: number;
  warnings?: string[];
  build_secs: number;
  curl_hint: string;
}

function text(s: string, isError = false) {
  return { content: [{ type: "text" as const, text: s }], isError };
}

function renderGuestErrors(errors: GuestError[] | undefined): string {
  if (!errors || errors.length === 0) return "";
  return (
    "\nRuntime errors from the guest:\n" +
    errors
      .map((e) => {
        let s = `[${e.kind}] ${e.message}`;
        if (e.detail && e.detail !== e.message) s += `\n${e.detail}`;
        if (e.hint) s += `\n(hint: ${e.hint})`;
        return s;
      })
      .join("\n\n")
  );
}

async function packDirectory(dir: string): Promise<Buffer> {
  const chunks: Buffer[] = [];
  const stream = tar.create(
    {
      gzip: true,
      cwd: dir,
      portable: true,
      filter: (p) => p !== ".git" && !p.startsWith(".git/") && !p.includes("/.git/"),
    },
    ["."]
  );
  for await (const chunk of stream) chunks.push(chunk as Buffer);
  return Buffer.concat(chunks);
}

function sanitizeName(base: string): string {
  const n = base
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 31);
  return n || "app";
}

const server = new McpServer({ name: "krill", version: "0.1.0" });

server.registerTool(
  "deploy",
  {
    description:
      "Deploy a directory as a Krill app: docker build → microVM rootfs → registered app → URL. " +
      "The directory must contain a Dockerfile (a plain one — no init scripts or VM-specific files needed). " +
      "Deploying an existing app name replaces its code (and resets its data disk). " +
      "The result reports ready:true when the app booted and answered, or ready:false with the guest's " +
      "runtime errors so you can fix the code and deploy again.",
    inputSchema: {
      directory: z.string().describe("path to the app directory (must contain a Dockerfile)"),
      name: z
        .string()
        .optional()
        .describe("app name (DNS label; default: sanitized directory basename)"),
      guest_port: z
        .number()
        .int()
        .optional()
        .describe("port the app listens on (default: EXPOSE from the Dockerfile, else 8000)"),
      vcpus: z.number().int().optional().describe("vCPUs (default 1)"),
      mem_mib: z.number().int().optional().describe("RAM in MiB (default 512)"),
    },
  },
  async ({ directory, name, guest_port, vcpus, mem_mib }) => {
    const dir = resolve(directory);
    if (!existsSync(dir) || !statSync(dir).isDirectory()) {
      return text(`${dir} is not a directory`, true);
    }
    if (!existsSync(`${dir}/Dockerfile`)) {
      return text(`${dir} has no Dockerfile — krill deploys docker build contexts`, true);
    }
    const appName = name ?? sanitizeName(basename(dir));

    const params = new URLSearchParams();
    if (guest_port) params.set("guest_port", String(guest_port));
    if (vcpus) params.set("vcpus", String(vcpus));
    if (mem_mib) params.set("mem_mib", String(mem_mib));

    let body: Buffer;
    try {
      body = await packDirectory(dir);
    } catch (err) {
      return text(`packing ${dir}: ${err}`, true);
    }

    const resp = await fetch(`${ADMIN}/v1/apps/${appName}/deploy?${params}`, {
      method: "POST",
      headers: { "Content-Type": "application/gzip" },
      body: new Uint8Array(body),
      signal: AbortSignal.timeout(DEPLOY_TIMEOUT_MS),
    });

    if (resp.status === 422) {
      const e = (await resp.json()) as { stage?: string; build_log?: string; error?: string };
      return text(
        `Build failed at ${e.stage ?? "?"}: ${e.error ?? ""}\n\n--- build log ---\n${e.build_log ?? ""}`,
        true
      );
    }
    if (!resp.ok) {
      return text(`deploy failed: ${resp.status} ${await resp.text()}`, true);
    }
    const dr = (await resp.json()) as DeployResp;

    const head =
      `Deployed ${dr.app.name} (${dr.created ? "created" : "updated"}; ` +
      `image ${dr.size_mb} MB; build ${dr.build_secs}s)\n` +
      `URL: ${dr.url}\n` +
      `(no DNS for ${dr.url}? ${dr.curl_hint})` +
      (dr.warnings?.length ? `\nWarnings: ${dr.warnings.join("; ")}` : "");

    if (dr.ready) {
      return text(`${head}\nReady: yes — first wake ${dr.app.last_wake_ms} ms, state ${dr.app.state}.`);
    }
    return text(
      `${head}\nReady: NO — the app did not come up (${dr.wake_error ?? "unknown"}).` +
        renderGuestErrors(dr.errors) +
        `\n\nFix the app and call deploy again with the same name.`,
      true
    );
  }
);

server.registerTool(
  "logs",
  {
    description:
      "Fetch an app's runtime log (guest serial console): raw tail plus structured errors " +
      "(Python tracebacks, Node errors, panics). Use after a failed deploy or a 5xx from the app.",
    inputSchema: {
      name: z.string().describe("app name"),
      tail: z.number().int().optional().describe("lines to return (default 100)"),
    },
  },
  async ({ name, tail }) => {
    const resp = await fetch(`${ADMIN}/v1/apps/${name}/logs?tail=${tail ?? 100}`, {
      signal: AbortSignal.timeout(30_000),
    });
    if (!resp.ok) {
      return text(`logs failed: ${resp.status} ${await resp.text()}`, true);
    }
    const body = (await resp.json()) as { lines: string[] | null; errors: GuestError[] | null };
    const lines = body.lines ?? [];
    const out =
      (lines.length ? lines.join("\n") : "(log is empty — the app may never have booted)") +
      renderGuestErrors(body.errors ?? undefined);
    return text(out);
  }
);

server.registerTool(
  "apps",
  {
    description:
      "List all deployed apps with lifecycle state (ACTIVE/FROZEN/COLD/...), snapshot validity, and resources.",
    inputSchema: {},
  },
  async () => {
    const resp = await fetch(`${ADMIN}/v1/apps`, { signal: AbortSignal.timeout(30_000) });
    if (!resp.ok) {
      return text(`list failed: ${resp.status} ${await resp.text()}`, true);
    }
    return text(await resp.text());
  }
);

server.registerTool(
  "delete_app",
  {
    description: "Delete an app entirely: its VM, disk images, snapshots, and registration.",
    inputSchema: { name: z.string().describe("app name") },
  },
  async ({ name }) => {
    const resp = await fetch(`${ADMIN}/v1/apps/${name}`, {
      method: "DELETE",
      signal: AbortSignal.timeout(60_000),
    });
    if (!resp.ok && resp.status !== 204) {
      return text(`delete failed: ${resp.status} ${await resp.text()}`, true);
    }
    return text(`deleted ${name}`);
  }
);

const transport = new StdioServerTransport();
await server.connect(transport);
console.error(`krill-mcp on stdio (admin: ${ADMIN})`);
