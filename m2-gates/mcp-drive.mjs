#!/usr/bin/env node
// Scripted MCP client for gate B4: speaks JSON-RPC over stdio to the krill
// MCP server exactly the way Claude Code would — initialize, then one
// tools/call. Prints the tool result text; exits 1 if the tool reported an
// error. No dependencies.
//
// usage: mcp-drive.mjs <path-to-server.js> <tool> '<json-args>'
import { spawn } from "node:child_process";

const [serverPath, tool, argsJSON] = process.argv.slice(2);
if (!serverPath || !tool) {
  console.error("usage: mcp-drive.mjs <server.js> <tool> '<json-args>'");
  process.exit(2);
}

const child = spawn("node", [serverPath], {
  stdio: ["pipe", "pipe", "inherit"],
  env: process.env,
});

let buf = "";
const pending = new Map();
let nextId = 1;

child.stdout.on("data", (d) => {
  buf += d.toString();
  let nl;
  while ((nl = buf.indexOf("\n")) >= 0) {
    const line = buf.slice(0, nl);
    buf = buf.slice(nl + 1);
    if (!line.trim()) continue;
    const msg = JSON.parse(line);
    if (msg.id !== undefined && pending.has(msg.id)) {
      pending.get(msg.id)(msg);
      pending.delete(msg.id);
    }
  }
});

function rpc(method, params) {
  const id = nextId++;
  return new Promise((resolveP, rejectP) => {
    pending.set(id, (msg) => (msg.error ? rejectP(new Error(JSON.stringify(msg.error))) : resolveP(msg.result)));
    child.stdin.write(JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n");
    setTimeout(() => {
      if (pending.has(id)) {
        pending.delete(id);
        rejectP(new Error(`timeout waiting for ${method}`));
      }
    }, 16 * 60 * 1000).unref();
  });
}

try {
  await rpc("initialize", {
    protocolVersion: "2024-11-05",
    capabilities: {},
    clientInfo: { name: "b4-gate-driver", version: "0" },
  });
  child.stdin.write(JSON.stringify({ jsonrpc: "2.0", method: "notifications/initialized" }) + "\n");

  const result = await rpc("tools/call", { name: tool, arguments: JSON.parse(argsJSON ?? "{}") });
  for (const c of result.content ?? []) {
    if (c.type === "text") console.log(c.text);
  }
  child.kill();
  process.exit(result.isError ? 1 : 0);
} catch (err) {
  console.error("mcp-drive:", err.message);
  child.kill();
  process.exit(1);
}
