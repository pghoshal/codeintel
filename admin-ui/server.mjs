import { createServer } from "node:http";
import { createReadStream, statSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { extname, join, normalize } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL(".", import.meta.url));
const port = Number(process.env.CODEINTEL_ADMIN_PORT || 4177);
const apiBase = (process.env.CODEINTEL_API_BASE || "http://127.0.0.1:18080").replace(/\/+$/, "");
const artifactPath = process.env.CODEINTEL_ADMIN_ARTIFACT || "/tmp/codeintel-admin-ui-real-test.json";

const types = new Map([
  [".html", "text/html; charset=utf-8"],
  [".js", "text/javascript; charset=utf-8"],
  [".css", "text/css; charset=utf-8"],
  [".json", "application/json; charset=utf-8"],
  [".svg", "image/svg+xml"],
]);

function safeFilePath(urlPath) {
  let clean = decodeURIComponent(urlPath.split("?")[0] || "/");
  if (clean === "/") {
    clean = "/index.html";
  }
  clean = normalize(clean).replace(/^(\.\.[/\\])+/, "");
  if (clean.startsWith("/")) {
    clean = clean.slice(1);
  }
  return join(root, clean);
}

async function proxy(req, res) {
  const target = apiBase + req.url;
  const headers = new Headers(req.headers);
  headers.delete("host");
  try {
    const upstream = await fetch(target, {
      method: req.method,
      headers,
      body: req.method === "GET" || req.method === "HEAD" ? undefined : req,
      duplex: "half",
    });
    res.writeHead(upstream.status, Object.fromEntries(upstream.headers.entries()));
    if (upstream.body) {
      for await (const chunk of upstream.body) {
        res.write(chunk);
      }
    }
    res.end();
  } catch (error) {
    res.writeHead(502, { "content-type": "application/json; charset=utf-8" });
    res.end(JSON.stringify({ error: "admin proxy failed", detail: String(error?.message || error) }));
  }
}

const server = createServer((req, res) => {
  if (req.url?.startsWith("/api/")) {
    proxy(req, res);
    return;
  }
  if ((req.url || "").split("?")[0] === "/admin-artifacts/latest") {
    readFile(artifactPath, "utf8").then((body) => {
      res.writeHead(200, { "content-type": "application/json; charset=utf-8", "cache-control": "no-store" });
      res.end(body);
    }).catch((error) => {
      res.writeHead(404, { "content-type": "application/json; charset=utf-8", "cache-control": "no-store" });
      res.end(JSON.stringify({ error: "artifact not found", path: artifactPath, detail: String(error?.message || error) }));
    });
    return;
  }
  const filePath = safeFilePath(req.url || "/");
  try {
    const st = statSync(filePath);
    if (!st.isFile()) {
      throw new Error("not a file");
    }
    res.writeHead(200, { "content-type": types.get(extname(filePath)) || "application/octet-stream" });
    createReadStream(filePath).pipe(res);
  } catch {
    res.writeHead(404, { "content-type": "text/plain; charset=utf-8" });
    res.end("not found");
  }
});

server.listen(port, "127.0.0.1", () => {
  console.log(`codeintel admin console: http://127.0.0.1:${port}`);
  console.log(`proxying /api/* to ${apiBase}`);
});
