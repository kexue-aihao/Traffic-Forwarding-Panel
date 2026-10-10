// Real panel/SQLite UI contract checks; forwarding is covered by Go socket tests.
import {
  chromium,
  firefox,
  webkit,
} from "../web/node_modules/@playwright/test/index.mjs";
import { execFileSync, spawn } from "node:child_process";
import { mkdir } from "node:fs/promises";
import { resolve } from "node:path";
import { randomUUID } from "node:crypto";
import assert from "node:assert/strict";

const root = resolve(import.meta.dirname, "..");
const directory = resolve(root, ".local");
await mkdir(directory, { recursive: true });
const binary = resolve(
  directory,
  process.platform === "win32"
    ? "tls-ingress-ui-panel.exe"
    : "tls-ingress-ui-panel",
);
const database = resolve(directory, `tls-ingress-ui-${randomUUID()}.db`);
const password = randomUUID();
const base = "http://127.0.0.1:18083";
execFileSync("go", ["build", "-o", binary, "./cmd/panel"], {
  cwd: root,
  windowsHide: true,
});
execFileSync(binary, ["-dsn", database, "-init-admin", "tls-admin"], {
  input: password + "\n",
  windowsHide: true,
  stdio: ["pipe", "pipe", "pipe"],
});
const service = spawn(
  binary,
  ["-addr", "127.0.0.1:18083", "-dsn", database, "-origin", base],
  { windowsHide: true, stdio: ["ignore", "pipe", "pipe"] },
);
const startup = new Promise((resolve, reject) => {
  service.stderr.on("data", (data) => {
    if (String(data).includes("面板监听")) resolve();
  });
  service.once("exit", (code) => reject(Error(`test panel exited ${code}`)));
});
async function pick(page, label, value) {
  const select = page.getByLabel(label, { exact: true });
  await select.waitFor();
  const values = await select
    .locator("option")
    .evaluateAll((options) => options.map((o) => o.value));
  assert.ok(values.includes(value), `missing option ${label}:${value}`);
  await select.click();
  await page
    .locator(".select-list .select-option")
    .nth(values.indexOf(value))
    .click();
}
try {
  await Promise.race([
    startup,
    new Promise((_, reject) => {
      const timer = setTimeout(() => reject(Error("startup timeout")), 10000);
      timer.unref();
    }),
  ]);
  for (const [name, engine] of Object.entries({ chromium, firefox, webkit })) {
    const browser = await engine.launch({ headless: true });
    try {
      const page = await browser.newPage();
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      await page.goto(base + "/admin");
      await page.getByLabel("用户名", { exact: true }).fill("tls-admin");
      await page.getByLabel("密码", { exact: true }).fill(password);
      await page.getByRole("button", { name: "登录控制台", exact: true }).click();
      await page.getByRole("link", { name: "设备组", exact: true }).waitFor();
      const headers = { "X-Requested-With": "fetch" };
      const response = await page.request.post(base + "/api/v1/groups", {
        headers,
        data: {
          name: `tls-${name}`,
          type: "entry",
          direct_policy: "allow",
          port_min: 20000,
          port_max: 21000,
        },
      });
      assert.equal(response.status(), 201);
      const group = await response.json();
      const enrolled = await page.request.post(
        base + "/api/v1/nodes/enrollment",
        { headers, data: { name: `node-${name}`, group_ids: [group.id] } },
      );
      const enrollment = await enrolled.json();
      const registeredResponse = await page.request.post(
        base + "/api/v1/agent/register",
        {
          data: {
            token: enrollment.token,
            name: `node-${name}`,
            capabilities: [
              "tcp",
              "direct",
              "advanced-routing-v1",
              "shared-tls-ingress-v1",
              "group-policy-v2",
              "inbound-inspection-v1",
              "http-stream-filter-v1",
              "peer-address-policy-v1",
              "route-failover-v1",
            ],
          },
        },
      );
      assert.equal(registeredResponse.status(), 201);
      const node = await registeredResponse.json();
      await page.getByRole("link", { name: "设备组", exact: true }).click();
      const row = page.getByRole("row").filter({ hasText: `tls-${name}` });
      await row.getByRole("button", { name: "高级设置", exact: true }).click();
      await pick(page, "添加额外参数", "shared_tls_ingress");
      await page.getByRole("button", { name: "添加参数", exact: true }).click();
      await page.getByLabel("启用共享入口", { exact: true }).check();
      await page.getByLabel("监听 IP", { exact: true }).fill("0.0.0.0");
      await page.getByLabel("共享 TCP 端口", { exact: true }).fill("8443");
      await page
        .getByRole("button", { name: "保存高级设置", exact: true })
        .click();
      await page.locator("dialog").waitFor({ state: "detached" });
      await page.getByRole("link", { name: "转发规则", exact: true }).click();
      await page.getByRole("button", { name: "新增", exact: true }).click();
      await page.getByLabel("名称", { exact: true }).fill(`shared-${name}`);
      await pick(page, "入口服务器", `${node.node_id}::${group.id}`);
      assert.equal(
        await page.getByLabel("按 SNI 路由").isChecked(),
        true,
        "new rules default to group ingress",
      );
      assert.equal(
        await page.getByLabel("监听地址", { exact: true }).count(),
        0,
      );
      assert.equal(
        await page.getByLabel("母规则 ID", { exact: true }).count(),
        0,
      );
      await page.getByLabel("目标地址", { exact: true }).fill("127.0.0.1:9443");
      await page
        .getByLabel("匹配域名", { exact: true })
        .fill(`${name}.example.com`);
      // Drafts avoid billing fixtures; enabled forwarding uses real socket tests.
      await page.getByLabel("启用规则", { exact: true }).uncheck();
      const savedPromise = page.waitForResponse(
        (r) =>
          r.url().endsWith("/api/v1/rules") && r.request().method() === "POST",
      );
      await page.getByRole("button", { name: "保存", exact: true }).click();
      const savedResponse = await savedPromise;
      assert.equal(savedResponse.status(), 201);
      const shared = await savedResponse.json();
      assert.equal(shared.listen, "0.0.0.0:8443");
      assert.ok(
        shared.shared_tls.ingress_id &&
          shared.shared_tls.ingress_id !== "group",
      );
      await page.locator("dialog").waitFor({ state: "detached" });
      await page
        .getByRole("row")
        .filter({ hasText: `shared-${name}` })
        .getByRole("button", { name: "编辑", exact: true })
        .click();
      await page.getByLabel("目标地址", { exact: true }).fill("127.0.0.1:9444");
      await page.getByRole("button", { name: "保存", exact: true }).click();
      await page.locator("dialog").waitFor({ state: "detached" });
      await page.getByRole("button", { name: "新增", exact: true }).click();
      await pick(page, "入口服务器", `${node.node_id}::${group.id}`);
      await page.getByLabel("按 SNI 路由").uncheck();
      await page.getByLabel("监听地址", { exact: true }).waitFor();
      await page
        .getByLabel("名称", { exact: true })
        .fill(`independent-${name}`);
      await page.getByLabel("启用规则", { exact: true }).uncheck();
      await page.getByLabel("目标地址", { exact: true }).fill("127.0.0.1:9443");
      await page.getByRole("button", { name: "保存", exact: true }).click();
      await page.locator("dialog").waitFor({ state: "detached" });
      const listed = await (
        await page.request.get(base + "/api/v1/rules?page_size=100")
      ).json();
      const independent = listed.items.find(
        (r) => r.name === `independent-${name}`,
      );
      assert.ok(
        !independent.shared_tls &&
          Number(independent.listen.split(":").at(-1)) >= 20000,
      );
      assert.deepEqual(errors, []);
      console.log(
        `${name}: shared TLS settings, default SNI route, edit, independent override PASS`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  service.kill();
}
