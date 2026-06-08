# codeintel admin console

This is a standalone verification console for humans. It is not the
production control plane: Atom and MCP/harness clients remain the real
control surfaces. The console consumes the public `codeintel-app` APIs so
operators can validate tenants, VCS connections, branch indexing, search,
chat, and MCP behavior without touching the database.

## Run

```bash
cd codeintel/admin-ui
CODEINTEL_API_BASE=http://127.0.0.1:18080 node server.mjs
```

Open http://127.0.0.1:4177.

The server is a tiny static-file host plus `/api/*` proxy. It avoids CORS
when the API is running in Kind through `kubectl port-forward`.

## Supported now

- Atom-style workspace provisioning through `POST /api/atom/workspaces`.
- Per-workspace API key/domain storage in browser local storage.
- Operations overview backed by `/api/health`, `/api/version`,
  `/api/status`, tenant metadata, secrets, models, connections, repos, and
  search contexts.
- Per-workspace secret creation/deletion and language-model upsert through
  `/api/secrets` and `/api/models`. Model saves preserve existing models and
  store only secret references.
- Generic Git VCS connection creation, connection sync, and status polling.
- Repo list and per-repo/per-branch status.
- Branch policy update for selected repos.
- Index/reindex and remove-index for one branch or a comma-separated branch
  list.
- Search-context creation from selected repos through `/api/search-contexts`.
- Direct `/api/search` test panel for comparing plain search against MCP and
  chat.
- Chat through `/api/chat/blocking`, `/api/chat`, and
  `/api/chat/{id}/result`.
- Chat model/context selectors, scoped repo chips, durable chat id display,
  Markdown rendering, Mermaid rendering when the CDN loads, clickable file
  references, and right-panel code reads through MCP `read_file`.
- Chat artifact loading from `/admin-artifacts/latest` for real product-flow
  proof runs saved by automation. Default artifact path:
  `/tmp/codeintel-admin-ui-real-test.json`.
- MCP request lab for direct `tools/call` testing with selected-scope presets.

## Honest product gap

The app-based GitHub/GitLab/Bitbucket OAuth flow requested for production is
not yet exposed as a codeintel API. The console shows the required provider
cards but does not fake authorization. The needed backend surface is:

- `GET /api/vcs/apps/{provider}/authorize?workspaceId=...`
- `GET /api/vcs/apps/{provider}/callback`
- `GET /api/vcs/connections/{id}/repos`
- token refresh/storage owned by Atom/codeintel integration, with org scope
  on every token and repo read.

Until those endpoints exist, the working verification path is the current
generic Git connection flow.

The code panel still needs repo-aware file-reference metadata from MCP/chat
answers for perfect cross-repo click-through. Today it opens file references
against the selected repo/default ref, which is safe for single-repo checks and
usable for multi-repo tests only when the selected repo matches the cited file.
