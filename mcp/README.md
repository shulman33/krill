# krill-mcp

MCP server for [Krill](../README.md): gives an agent (Claude Code, or any
MCP client) the deploy loop and the front door as tools — "agent-written apps
deploy in one call and share like a Google Doc" is one sentence, so it is one
tool surface.

| Tool | What it does |
|---|---|
| `deploy` | directory → Docker build → microVM rootfs → registered app → **URL**, with a post-deploy verification wake. `ready: false` comes back with the guest's structured runtime errors. |
| `logs` | serial-console tail + parsed errors (Python tracebacks, Node errors, panics) |
| `apps` | list apps and lifecycle states |
| `delete_app` | remove an app |
| `share` | mint a capability link for one app and one plane (`use`/`data`/`edit`). Returned once — only its hash is stored. |
| `shares` | the ACL: live links, who claimed them, what was revoked |
| `unshare` | revoke a person or a link. In force on the next request, durable before the call returns; an error means **nothing** was revoked. |

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

Environment: `KRILL_ADMIN` (default `http://127.0.0.1:9091`) for the deploy
tools, and `KRILL_DOORMAN` (default: that port + 1, i.e. `:9092`) for the
sharing tools — they talk to `krill-doorman`, not krilld. A tunnel therefore
needs to forward **both** ports:

```sh
ssh -L 9091:127.0.0.1:9091 -L 9092:127.0.0.1:9092 <host>
```

Then, in a session: *"deploy ./my-app"* — one tool call in, running app +
URL out. A broken app comes back with its traceback in the tool result;
fix and deploy again under the same name.
