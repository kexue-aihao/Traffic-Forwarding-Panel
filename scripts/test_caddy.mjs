// Real Caddy TLS -> real panel/YAML/SQLite smoke test. Uses only localhost,
// a private test CA and simulated node registration; never installs CA trust.
import assert from "node:assert/strict";
import { spawn, execFileSync } from "node:child_process";
import { randomBytes, randomUUID, createHash } from "node:crypto";
import { mkdir, readFile, writeFile, readdir } from "node:fs/promises";
import { createServer } from "node:net";
import https from "node:https";
import { resolve, join } from "node:path";
import { setTimeout as delay } from "node:timers/promises";

const root = resolve(import.meta.dirname, "..");
const work = resolve(root, ".local", `caddy-test-${randomUUID()}`);
await mkdir(work, { recursive: true });
const caddy = process.env.TFP_TEST_CADDY || "caddy";
const binary = join(work, process.platform === "win32" ? "panel.exe" : "panel");
const cleanEnv = { ...process.env };
for (const key of Object.keys(cleanEnv))
  if (key.startsWith("TFP_")) delete cleanEnv[key];
const env = {
  ...cleanEnv,
  XDG_DATA_HOME: join(work, "data"),
  XDG_CONFIG_HOME: join(work, "config"),
};
const run = (file, args, extra = {}) =>
  execFileSync(file, args, {
    cwd: root,
    env,
    windowsHide: true,
    stdio: ["pipe", "pipe", "pipe"],
    ...extra,
  });
run(caddy, ["version"]);
run("go", ["build", "-o", binary, "./cmd/panel"]);
async function freePort() {
  const server = createServer();
  await new Promise((done) => server.listen(0, "127.0.0.1", done));
  const port = server.address().port;
  await new Promise((done) => server.close(done));
  return port;
}
const backendPort = await freePort();
const tlsPort = await freePort();
const base = `https://localhost:${tlsPort}`;
const config = join(work, "panel.yaml");
const password = randomUUID();
await writeFile(
  config,
  `database-path: sqlite3://panel.db\nlisten: 127.0.0.1:${backendPort}\ntrust-proxy: true\ndisable-gzip: true\nmax-open-connection: 4\nmax-idle-connection: 1\nuser-rate-limit:\n  rate: 5\n  limit: 100\ndefault-rate-limit:\n  rate: 5\n  limit: 50\n`,
);
run(binary, ["-config", config, "-check-config"]);
run(binary, ["-config", config, "-init-admin", "caddy-admin"], {
  input: password + "\n",
});
const publicDir = join(work, "public");
run(binary, ["-export-html", publicDir]);
let service;
let proxy;
let ca;
let cookie = "";
function start(file, args) {
  const child = spawn(file, args, {
    cwd: work,
    env,
    windowsHide: true,
    stdio: ["ignore", "pipe", "pipe"],
  });
  child.output = "";
  for (const stream of [child.stdout, child.stderr])
    stream.on("data", (chunk) => {
      child.output = (child.output + chunk).slice(-10000);
    });
  return child;
}
async function stop(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  const exited = new Promise((done) => child.once("exit", done));
  child.kill();
  await Promise.race([exited, delay(3000, undefined, { ref: false })]);
  if (child.exitCode === null && child.signalCode === null)
    child.kill("SIGKILL");
}
async function findRoot(directory) {
  for (const entry of await readdir(directory, { withFileTypes: true }).catch(
    () => [],
  )) {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) {
      const found = await findRoot(path);
      if (found) return found;
    } else if (entry.name === "root.crt") return readFile(path);
  }
}
function request(path, method = "GET", body, headers = {}) {
  return new Promise((done, reject) => {
    const data = body === undefined ? undefined : JSON.stringify(body);
    const req = https.request(
      base + path,
      {
        method,
        ca,
        family: 4,
        timeout: 5000,
        headers: {
          Origin: base,
          "X-Requested-With": "fetch",
          ...(cookie ? { Cookie: cookie } : {}),
          ...(data ? { "Content-Type": "application/json" } : {}),
          ...headers,
        },
      },
      (res) => {
        const chunks = [];
        res.on("data", (chunk) => chunks.push(chunk));
        res.on("end", () =>
          done({
            status: res.statusCode,
            headers: res.headers,
            text: Buffer.concat(chunks).toString(),
          }),
        );
      },
    );
    req.on("error", reject);
    req.on("timeout", () => req.destroy(Error("HTTPS request timeout")));
    req.end(data);
  });
}
async function json(path, method, body, expected = 200, headers) {
  const result = await request(path, method, body, headers);
  assert.equal(
    result.status,
    expected,
    `${method || "GET"} ${path}: ${result.text}`,
  );
  return result.text ? JSON.parse(result.text) : null;
}
async function sse() {
  await new Promise((done, reject) => {
    const req = https.get(
      base + "/api/v1/probes/events",
      { ca, family: 4, headers: { Cookie: cookie }, timeout: 5000 },
      (res) => {
        if (
          res.statusCode !== 200 ||
          !res.headers["content-type"].includes("text/event-stream")
        ) {
          reject(Error("SSE response invalid"));
          req.destroy();
          return;
        }
        res.once("data", (chunk) => {
          try {
            assert.match(chunk.toString(), /event: probes/);
            done();
          } catch (e) {
            reject(e);
          } finally {
            req.destroy();
          }
        });
      },
    );
    req.on("error", reject);
    req.on("timeout", () => req.destroy(Error("SSE buffering timeout")));
  });
}
async function websocket(path, origin, expected) {
  await new Promise((done, reject) => {
    const key = randomBytes(16).toString("base64");
    const req = https.request(base + path, {
      ca,
      family: 4,
      timeout: 5000,
      headers: {
        Cookie: cookie,
        Origin: origin,
        Upgrade: "websocket",
        Connection: "Upgrade",
        "Sec-WebSocket-Version": "13",
        "Sec-WebSocket-Key": key,
      },
    });
    req.on("upgrade", (res, socket) => {
      try {
        assert.equal(res.statusCode, expected);
        assert.equal(
          res.headers["sec-websocket-accept"],
          createHash("sha1")
            .update(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
            .digest("base64"),
        );
        done();
      } catch (e) {
        reject(e);
      } finally {
        socket.destroy();
      }
    });
    req.on("response", (res) => {
      res.resume();
      try {
        assert.equal(res.statusCode, expected);
        done();
      } catch (e) {
        reject(e);
      }
    });
    req.on("error", reject);
    req.on("timeout", () => req.destroy(Error("WebSocket timeout")));
    req.end();
  });
}
try {
  service = start(binary, ["-config", config]);
  for (let i = 0; i < 100; i++) {
    try {
      if ((await fetch(`http://127.0.0.1:${backendPort}/api/v1/health`)).ok)
        break;
    } catch {}
    if (service.exitCode !== null) throw Error(service.output);
    await delay(100);
  }
  run(binary, ["-config", config, "-healthcheck"]);
  for (const mode of ["proxy", "static"]) {
    const source = await readFile(
      join(
        root,
        "deploy/caddy",
        mode === "proxy" ? "Caddyfile" : "Caddyfile.static",
      ),
      "utf8",
    );
    const contents =
      "{\n admin off\n skip_install_trust\n auto_https disable_redirects\n}\n" +
      source
        .replace(
          "panel.example.com {",
          `${base} {\n tls internal\n bind 127.0.0.1`,
        )
        .replaceAll("127.0.0.1:18888", `127.0.0.1:${backendPort}`)
        .replaceAll(
          "/srv/traffic-forwarding-panel/public",
          '"' + publicDir.replaceAll("\\", "/") + '"',
        );
    const caddyfile = join(work, "Caddyfile");
    await writeFile(caddyfile, contents);
    proxy = start(caddy, [
      "run",
      "--config",
      caddyfile,
      "--adapter",
      "caddyfile",
    ]);
    let ready = false;
    for (let i = 0; i < 150; i++) {
      ca ||= await findRoot(work);
      if (ca) {
        try {
          if ((await request("/api/v1/health")).status === 200) {
            ready = true;
            break;
          }
        } catch {}
      }
      if (proxy.exitCode !== null) throw Error(proxy.output);
      await delay(100);
    }
    assert.ok(ready, `Caddy did not start: ${proxy.output}`);
    for (const path of [
      "/",
      "/admin",
      "/admin/",
      "/assets/app.js",
      "/assets/app.css",
    ]) {
      const res = await request(path);
      assert.equal(res.status, 200, path);
      assert.ok(res.headers["content-security-policy"], "CSP missing");
    }
    for (const path of [
      "/config.yaml",
      "/panel.db",
      "/assets/missing.js",
      "/api/v1/missing",
    ])
      assert.equal((await request(path)).status, 404, path);
    assert.match(
      (await request("/api/v1/missing")).headers["content-type"],
      /json/,
    );
    const login = await request(
      "/api/v1/auth/login",
      "POST",
      { username: "caddy-admin", password },
      { "X-Forwarded-Proto": "http", "X-Real-IP": "203.0.113.99" },
    );
    assert.equal(login.status, 200);
    const setCookie = login.headers["set-cookie"][0];
    assert.match(setCookie, /; Secure/);
    assert.match(setCookie, /; HttpOnly/);
    cookie = setCookie.split(";")[0];
    await json("/api/v1/auth/session");
    assert.equal(
      (
        await request(
          "/api/v1/auth/login",
          "POST",
          { username: "caddy-admin", password },
          { Origin: "https://unrelated.example" },
        )
      ).status,
      403,
    );
    await sse();
    const group = await json(
      "/api/v1/groups",
      "POST",
      {
        name: `caddy-${mode}`,
        type: "entry",
        port_min: 10000,
        port_max: 60000,
      },
      201,
    );
    const enrollment = await json(
      "/api/v1/nodes/enrollment",
      "POST",
      { name: `simulated-${mode}`, group_ids: [group.id] },
      201,
    );
    const node = await json(
      "/api/v1/agent/register",
      "POST",
      {
        name: `simulated-${mode}`,
        token: enrollment.token,
        capabilities: ["terminal-v1"],
      },
      201,
    );
    await json("/api/v1/agent/config", "GET", undefined, 200, {
      Authorization: `Bearer ${node.token}`,
    });
    const grant = await json(
      `/api/v1/nodes/${node.node_id}/operation-access`,
      "POST",
      { password },
      201,
    );
    const terminal = await json(
      `/api/v1/nodes/${node.node_id}/terminal`,
      "POST",
      { access_token: grant.token, idempotency_key: randomUUID() },
      201,
    );
    await websocket(
      `/api/v1/node-operations/${terminal.id}/terminal`,
      "https://unrelated.example",
      403,
    );
    await websocket(
      `/api/v1/node-operations/${terminal.id}/terminal`,
      base,
      101,
    );
    // No remote shell is attached or executed; only the proxy upgrade is checked.
    const installer = await request(
      "/download/agent-install.sh",
      "GET",
      undefined,
      { Cookie: "" },
    );
    assert.equal(installer.status, 200);
    assert.match(installer.text, /^#!/);
    assert.equal(
      (
        await request("/online/device/ip/list", "GET", undefined, {
          Cookie: "",
        })
      ).status,
      401,
    );
    console.log(
      `Caddy ${mode}: PASS (verified HTTPS, YAML/SQLite, frontend, Secure cookie, CSRF, SSE, WebSocket upgrade, Agent/download/online routes)`,
    );
    await stop(proxy);
    proxy = undefined;
    cookie = "";
  }
} finally {
  await stop(proxy);
  await stop(service);
}
