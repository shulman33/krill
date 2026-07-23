# krill-mcp

MCP server for [Krill](../README.md): gives an agent (Claude Code, or any
MCP client) the M2 deploy loop as tools.

| Tool | What it does |
|---|---|
| `deploy` | directory → Docker build → microVM rootfs → registered app → **URL**, with a post-deploy verification wake. `ready: false` comes back with the guest's structured runtime errors. |
| `logs` | serial-console tail + parsed errors (Python tracebacks, Node errors, panics) |
| `apps` | list apps and lifecycle states |
| `delete_app` | remove an app |

## Build

```sh
npm install
npm run build      # → dist/index.js
```

## Hook up to Claude Code

On the krilld host (or with `ssh -L 9091:127.0.0.1:9091 <host>` running):

```sh
claude mcp add krill -- node /path/to/krill/mcp/dist/index.js
```

Environment: `KRILL_ADMIN` (default `http://127.0.0.1:9091`).

Then, in a session: *"deploy ./my-app"* — one tool call in, running app +
URL out. A broken app comes back with its traceback in the tool result;
fix and deploy again under the same name.
