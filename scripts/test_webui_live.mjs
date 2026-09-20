// Real Go + SQLite + embedded UI integration. Agent registration/probe are
// explicitly simulated HTTP clients, not evidence of an actual forwarding node.
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
await mkdir(resolve(root, ".local"), { recursive: true });
const binary = resolve(
  root,
  ".local",
  process.platform === "win32" ? "ui-live-panel.exe" : "ui-live-panel",
);
const database = resolve(root, ".local", `ui-live-${randomUUID()}.db`);
const password = randomUUID();
execFileSync("go", ["build", "-o", binary, "./cmd/panel"], {
  cwd: root,
  windowsHide: true,
});
execFileSync(binary, ["-dsn", database, "-init-admin", "ui-admin"], {
  input: password + "\n",
  windowsHide: true,
  stdio: ["pipe", "pipe", "pipe"],
});
const base = "http://127.0.0.1:18081";
const service = spawn(
  binary,
  ["-addr", "127.0.0.1:18081", "-dsn", database, "-origin", base],
  { windowsHide: true, stdio: ["ignore", "pipe", "pipe"] },
);
const startup = new Promise((resolve, reject) => {
  service.stderr.on("data", (data) => {
    if (String(data).includes("面板监听")) resolve();
  });
  service.once("exit", (code) =>
    reject(new Error(`test panel exited ${code}`)),
  );
});
await Promise.race([
  startup,
  new Promise((_, reject) => {
    const timer = setTimeout(() => reject(Error("startup timeout")), 10000);
    timer.unref();
  }),
]);
async function login(page, username, secret, path = "/") {
  await page.goto(base + path);
  await page.getByLabel("用户名", { exact: true }).fill(username);
  await page.getByLabel("密码", { exact: true }).fill(secret);
  await page.getByRole("button", { name: "登录控制台" }).click();
  await page.getByRole("heading", { name: `你好，${username}` }).waitFor();
}
async function save(page) {
  await page.getByRole("button", { name: "保存", exact: true }).click();
  await page.locator("dialog").waitFor({ state: "detached" });
}
try {
  for (const [browserName, engine] of Object.entries({
    chromium,
    firefox,
    webkit,
  })) {
    const browser = await engine.launch({ headless: true });
    try {
      const admin = await browser.newPage();
      const exceptions = [];
      admin.on("pageerror", (e) => exceptions.push(e.message));
      await login(admin, "ui-admin", password, "/admin");
      const username = `ui-${browserName}`;
      const userPassword = randomUUID();
      await admin.getByRole("link", { name: "用户管理", exact: true }).click();
      await admin.getByRole("button", { name: "新增", exact: true }).click();
      await admin.getByLabel("用户名", { exact: true }).fill(username);
      await admin.getByLabel("初始密码", { exact: true }).fill(userPassword);
      await save(admin);
      await admin.getByRole("link", { name: "设备组", exact: true }).click();
      await admin.getByRole("button", { name: "新增", exact: true }).click();
      await admin
        .getByLabel("名称", { exact: true })
        .fill(`group-${browserName}`);
      await admin.getByLabel(username, { exact: true }).check();
      await admin
        .getByRole("group", { name: "屏蔽隧道承载", exact: true })
        .getByLabel("WS", { exact: true })
        .check();
      await admin
        .getByRole("group", { name: "屏蔽明文应用协议", exact: true })
        .getByLabel("SOCKS 应用流量", { exact: true })
        .check();
      await save(admin);
      const savedGroups = await (
        await admin.request.get(base + "/api/v1/groups?page_size=100")
      ).json();
      const savedGroup = savedGroups.items.find(
        (group) => group.name === `group-${browserName}`,
      );
      assert.deepEqual(savedGroup.blocked_protocols, [
        "transport:ws",
        "app:socks",
      ]);
      await admin.getByRole("link", { name: "服务器", exact: true }).click();
      await admin.getByRole("button", { name: "生成接入凭据" }).click();
      await admin
        .getByLabel("名称", { exact: true })
        .fill(`simulated-${browserName}`);
      await admin.getByLabel(`group-${browserName}`, { exact: true }).check();
      await admin.getByRole("button", { name: "保存", exact: true }).click();
      const enrollment = (
        await admin.getByLabel("一次性接入凭据").inputValue()
      ).split("\n")[0];
      assert.ok(enrollment.length > 20);
      await admin.getByRole("button", { name: "已保存，关闭" }).click();
      const registration = await admin.request.post(
        base + "/api/v1/agent/register",
        {
          data: {
            token: enrollment,
            name: `simulated-${browserName}`,
            agent_version: "ui-test-simulation",
            os: "simulated",
            arch: "simulated",
            capabilities: ["tcp", "direct"],
          },
        },
      );
      assert.equal(registration.status(), 201);
      const registered = await registration.json();
      const probe = await admin.request.post(base + "/api/v1/agent/probe", {
        headers: { Authorization: `Bearer ${registered.token}` },
        data: {
          node_id: registered.node_id,
          sampled_at: new Date().toISOString(),
          cpu_percent: null,
          memory_used: null,
          memory_total: null,
          disk_used: null,
          disk_total: null,
          upload_bps: null,
          download_bps: null,
          uptime_seconds: null,
          load1: null,
          public_ips: [
            {
              address: "203.0.113.99",
              family: "ipv4",
              source: "ui-test-simulation",
              observed_at: new Date().toISOString(),
            },
          ],
        },
      });
      assert.equal(probe.status(), 204);
      await admin
        .getByRole("link", { name: "套餐与钱包", exact: true })
        .click();
      await admin.getByRole("button", { name: "新增套餐" }).click();
      await admin
        .getByLabel("名称", { exact: true })
        .fill(`plan-${browserName}`);
      await admin.getByRole("button", { name: "确认提交" }).click();
      await admin.locator("dialog").waitFor({ state: "detached" });
      assert.equal(
        await admin
          .getByRole("button", { name: "充值钱包", exact: true })
          .isDisabled(),
        true,
      );
      const userContext = await browser.newContext();
      const user = await userContext.newPage();
      user.on("pageerror", (e) => exceptions.push(e.message));
      await login(user, username, userPassword);
      assert.equal(
        await user.getByRole("link", { name: "用户管理", exact: true }).count(),
        0,
      );
      assert.equal(
        (await user.request.get(base + "/api/v1/users")).status(),
        403,
      );
      await user.getByRole("link", { name: "实时探针", exact: true }).click();
      await user
        .getByRole("heading", { name: `simulated-${browserName}` })
        .waitFor();
      assert.equal(
        (await user.locator("body").innerText()).includes("203.0.113.99"),
        false,
      );
      const userNodes = await (
        await user.request.get(base + "/api/v1/nodes")
      ).json();
      assert.equal(userNodes.items.length, 1);
      await user.getByRole("link", { name: "转发规则", exact: true }).click();
      await user.getByRole("button", { name: "新增", exact: true }).click();
      await user
        .getByLabel("名称", { exact: true })
        .fill("disabled-integration-rule");
      await user.getByLabel("入口服务器").selectOption(registered.node_id);
      const userGroups = await (
        await user.request.get(base + "/api/v1/groups")
      ).json();
      await user.getByLabel("设备组").selectOption(userGroups.items[0].id);
      await user.getByLabel("目标地址", { exact: true }).fill("127.0.0.1:8080");
      await user.getByLabel("启用规则", { exact: true }).uncheck();
      await save(user);
      await user.getByRole("button", { name: "编辑", exact: true }).click();
      assert.equal(await user.getByLabel("入口服务器").isDisabled(), true);
      assert.equal(
        await user.getByLabel("监听地址", { exact: true }).isDisabled(),
        true,
      );
      await user.getByLabel("目标地址", { exact: true }).fill("127.0.0.1:8081");
      await save(user);
      await user.getByRole("button", { name: "删除", exact: true }).click();
      await user.getByRole("button", { name: "确认删除", exact: true }).click();
      await user.locator("dialog").waitFor({ state: "detached" });
      await user.getByRole("link", { name: "套餐与钱包", exact: true }).click();
      await user.locator(".stat strong").filter({ hasText: "0.00" }).waitFor();
      await user
        .getByRole("button", { name: "余额购买", exact: true })
        .first()
        .click();
      await user.getByRole("button", { name: "确认提交" }).click();
      await user
        .getByText("可用余额不足，请核对钱包余额。", { exact: true })
        .waitFor();
      await user.getByRole("button", { name: "关闭对话框" }).click();
      await user.getByRole("link", { name: "账号与 API", exact: true }).click();
      await user.getByRole("button", { name: "创建 Token" }).click();
      await user
        .getByLabel("Token 名称", { exact: true })
        .fill("integration-key");
      await user.getByRole("button", { name: "确认提交" }).click();
      const apiToken = await user.getByLabel("API Token 密钥").inputValue();
      assert.ok(apiToken.length > 20);
      assert.equal(
        (
          await fetch(base + "/api/v1/nodes", {
            headers: { Authorization: `Bearer ${apiToken}` },
          })
        ).status,
        200,
      );
      await user.getByRole("button", { name: "已保存，关闭" }).click();
      assert.equal(
        (await admin.request.get(base + "/api/v1/auth/tokens")).status(),
        200,
      );
      const adminTokens = await (
        await admin.request.get(base + "/api/v1/auth/tokens")
      ).json();
      assert.equal(adminTokens.items.length, 0);
      await user.getByRole("button", { name: "撤销", exact: true }).click();
      await user.getByRole("button", { name: "确认撤销", exact: true }).click();
      await user.getByText("暂无 API Token。", { exact: true }).waitFor();
      assert.equal(
        (
          await fetch(base + "/api/v1/nodes", {
            headers: { Authorization: `Bearer ${apiToken}` },
          })
        ).status,
        401,
      );
      const nextPassword = randomUUID();
      await user.getByRole("button", { name: "修改密码", exact: true }).click();
      await user.getByLabel("当前密码", { exact: true }).fill(userPassword);
      await user.getByLabel("新密码", { exact: true }).fill(nextPassword);
      await user.getByLabel("确认新密码", { exact: true }).fill(nextPassword);
      await user.getByRole("button", { name: "确认提交" }).click();
      await user.getByRole("button", { name: "登录控制台" }).waitFor();
      await user.getByLabel("用户名", { exact: true }).fill(username);
      await user.getByLabel("密码", { exact: true }).fill(nextPassword);
      await user.getByRole("button", { name: "登录控制台" }).click();
      await user
        .getByRole("heading", { name: "账号与 API", exact: true })
        .waitFor();
      if (browserName === "chromium") {
        const screenshots = resolve(root, ".gocache/screens");
        await mkdir(screenshots, { recursive: true });
        for (const [role, page] of [
          ["admin", admin],
          ["user", user],
        ]) {
          const dismiss = page.getByRole("button", {
            name: "关闭通知",
            exact: true,
          });
          if (await dismiss.count()) await dismiss.click();
          for (const [section, label] of [
            ["overview", "概览"],
            ["commerce", "套餐与钱包"],
            ["account", "账号与 API"],
            ["probes", "实时探针"],
          ]) {
            await page.getByRole("link", { name: label, exact: true }).click();
            await page.waitForTimeout(180);
            for (const [viewport, size] of [
              ["desktop", { width: 1440, height: 1000 }],
              ["mobile", { width: 320, height: 800 }],
            ]) {
              await page.setViewportSize(size);
              await page.screenshot({
                path: resolve(
                  screenshots,
                  `${role}-${section}-${viewport}.png`,
                ),
              });
              assert.ok(
                await page.evaluate(
                  () => document.documentElement.scrollWidth <= innerWidth,
                ),
                "viewport overflow",
              );
              assert.ok(
                await page
                  .locator("#view")
                  .evaluate((el) => el.clientHeight > 0),
                "main is reachable",
              );
            }
          }
        }
        await admin.setViewportSize({ width: 1440, height: 1000 });
      }
      await admin.getByRole("link", { name: "用户管理", exact: true }).click();
      await admin
        .getByRole("row")
        .filter({ hasText: username })
        .getByRole("button", { name: "停用", exact: true })
        .click();
      await admin.getByRole("button", { name: "确认修改状态" }).click();
      await admin.locator("dialog").waitFor({ state: "detached" });
      await user.getByRole("link", { name: "服务器", exact: true }).click();
      await user.getByRole("button", { name: "登录控制台" }).waitFor();
      assert.deepEqual(exceptions, []);
      console.log(
        `${browserName}: REAL Go/SQLite/embedded UI PASS (login, user/group/enrollment, simulated Agent/probe privacy, zero wallet + insufficient funds, Token isolation/revocation, password, disable + session revoke)`,
      );
      await userContext.close();
    } finally {
      await browser.close();
    }
  }
} finally {
  service.kill();
}
