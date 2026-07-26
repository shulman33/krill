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
//   share       mint a capability link for one app and one plane (M4)
//   shares      the ACL: who has access, via which link, and what was revoked
//   unshare     revoke a person or a link, durably
//
// Config: KRILL_ADMIN (default http://127.0.0.1:9091) — krilld's admin API —
// and KRILL_DOORMAN (default: that port + 1) for the sharing tools, which
// talk to krill-doorman rather than krilld. Both are loopback-only, so run
// this on the host or bring an SSH tunnel that forwards both ports.
//
// The sharing tools exist here for the same reason `krill share` lives in the
// existing CLI (ROADMAP decision #10b): the krilld/doorman split is an
// implementation seam, and "agent-written apps deploy in one call and share
// like a Google Doc" is one sentence, not two tools.

import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";
import * as tar from "tar";
import { existsSync, statSync } from "node:fs";
import { basename, resolve } from "node:path";

const ADMIN = process.env.KRILL_ADMIN ?? "http://127.0.0.1:9091";

/** The doorman's control API: KRILL_DOORMAN, or the admin port + 1. */
const DOORMAN = process.env.KRILL_DOORMAN ?? (() => {
  const m = /^(https?:\/\/[^:]+):(\d+)$/.exec(ADMIN);
  return m ? `${m[1]}:${Number(m[2]) + 1}` : "http://127.0.0.1:9092";
})();
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

server.registerTool(
  "share",
  {
    description:
      "Create a share link for an app: an unguessable URL that is itself the grant. " +
      "The recipient opens it, signs in with Google, and is bound to the app's ACL — " +
      "you never need their address in advance. The link is returned ONCE (only its " +
      "hash is stored). Planes: use = send requests to the app, data = read/export " +
      "its /data, edit = replace its code (edit implies data implies use).",
    inputSchema: {
      app: z.string().describe("app name"),
      plane: z.enum(["use", "data", "edit"]).default("use").describe("what the link grants"),
      label: z.string().optional().describe("a note about who this link is for"),
      expires_in: z.string().optional().describe('Go duration, e.g. "72h"; omit for never'),
      max_claims: z.number().optional().describe("stop accepting new people after N claim it"),
    },
  },
  async ({ app, plane, label, expires_in, max_claims }) => {
    const resp = await fetch(`${DOORMAN}/v1/shares`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        app, plane, label: label ?? "", expires_in: expires_in ?? "",
        max_claims: max_claims ?? 0, created_by: "mcp",
      }),
      signal: AbortSignal.timeout(30_000),
    }).catch((e) => e as Error);
    if (resp instanceof Error) {
      return text(`cannot reach krill-doorman at ${DOORMAN}: ${resp.message}`, true);
    }
    if (!resp.ok) {
      return text(`share failed: ${resp.status} ${await resp.text()}`, true);
    }
    const body = (await resp.json()) as { share: { id: string; plane: string }; link: string };
    return text(
      `${body.link}\n\n` +
        `share ${body.share.id} on ${app} (${body.share.plane} plane).\n` +
        `Send that link to whoever should have access — it is shown once.\n` +
        `Revoke with: unshare {"app":"${app}","share":"${body.share.id}"}`
    );
  }
);

server.registerTool(
  "shares",
  {
    description:
      "Show the ACL: live share links and who has claimed them, per app or across all apps.",
    inputSchema: { app: z.string().optional().describe("limit to one app") },
  },
  async ({ app }) => {
    const q = app ? `?app=${encodeURIComponent(app)}` : "";
    const [shares, grants] = await Promise.all([
      fetch(`${DOORMAN}/v1/shares${q}`, { signal: AbortSignal.timeout(30_000) }),
      fetch(`${DOORMAN}/v1/grants${q}`, { signal: AbortSignal.timeout(30_000) }),
    ]).catch((e) => [e as Error, e as Error] as const);
    if (shares instanceof Error) {
      return text(`cannot reach krill-doorman at ${DOORMAN}: ${shares.message}`, true);
    }
    if (!shares.ok) return text(`shares failed: ${shares.status}`, true);
    return text(
      `links:\n${await shares.text()}\n\ngrants:\n${
        grants instanceof Error || !grants.ok ? "(unavailable)" : await grants.text()
      }`
    );
  }
);

server.registerTool(
  "unshare",
  {
    description:
      "Revoke access. Give `user` to cut off one person, or `share` to kill a link and " +
      "everyone who claimed it. Takes effect on the very next request — no restart, no " +
      "waiting out a session — and is durable at the object store before this returns, " +
      "so no restore can undo it. An error here means NOTHING was revoked.",
    inputSchema: {
      app: z.string().describe("app name"),
      user: z.string().optional().describe("email to revoke"),
      share: z.string().optional().describe("share id to revoke (sh_...)"),
      reason: z.string().optional(),
    },
  },
  async ({ app, user, share, reason }) => {
    const body = share
      ? { kind: "share", share, by: "mcp", reason: reason ?? "" }
      : { kind: "identity", app, email: user, by: "mcp", reason: reason ?? "" };
    if (!share && !user) {
      return text("give either `user` (an email) or `share` (a share id)", true);
    }
    const resp = await fetch(`${DOORMAN}/v1/revoke`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(60_000),
    }).catch((e) => e as Error);
    if (resp instanceof Error) {
      return text(`NOTHING WAS REVOKED — cannot reach krill-doorman: ${resp.message}`, true);
    }
    if (!resp.ok) {
      return text(`NOTHING WAS REVOKED: ${resp.status} ${await resp.text()}`, true);
    }
    const rv = (await resp.json()) as { id: string; grants?: string[] };
    return text(
      `revoked (${rv.id}), ${rv.grants?.length ?? 0} grant(s) cut off.\n` +
        `In force on the next request, and durable at the object store.`
    );
  }
);

const transport = new StdioServerTransport();
await server.connect(transport);
console.error(`krill-mcp on stdio (admin: ${ADMIN}, doorman: ${DOORMAN})`);
