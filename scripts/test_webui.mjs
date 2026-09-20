// Browser contract/visual regression with an isolated localhost fixture.
// This is not payment, Agent, database, or real-backend integration evidence.
import {
  chromium,
  firefox,
  webkit,
} from "../web/node_modules/@playwright/test/index.mjs";
import { createServer } from "node:http";
import { readFile, mkdir } from "node:fs/promises";
import { resolve, extname } from "node:path";
import assert from "node:assert/strict";
const assets = resolve(import.meta.dirname, "../internal/webui/assets");
let authorized = false;
let expire = false;
let rules = [];
let purchases = 0;
let paymentFixture = false;
let createdKeys = [];
let reconcileCalls = 0;
const uncertainOrder = {
  id: "uncertain-fixture",
  channel: "epay",
  amount_cents: "1000",
  currency: "CNY",
  status: "pending",
  created_at: new Date().toISOString(),
};
const user = {
  id: "u1",
  username: "fixture-admin",
  role: "admin",
  disabled: false,
};
const group = {
  id: "g1",
  name: "Fixture group",
  user_ids: ["u1"],
  blocked_protocols: [],
  multiplier: "1",
  port_min: 10000,
  port_max: 60000,
  version: 1,
};
const node = {
  id: "n1",
  name: "Fixture node",
  agent_version: "test",
  last_seen: new Date().toISOString(),
  desired_version: 1,
  applied_version: 1,
  apply_error: "",
};
const fixtureProbe = {
  node_id: "n1",
  sampled_at: new Date().toISOString(),
  cpu_percent: null,
  memory_used: null,
  memory_total: null,
  disk_used: null,
  disk_total: null,
  upload_bps: null,
  download_bps: null,
  load1: null,
  uptime_seconds: null,
};
const server = createServer(async (req, res) => {
  res.setHeader("X-WebUI-Preview", "contract-fixture");
  res.setHeader(
    "Content-Security-Policy",
    "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'",
  );
  res.setHeader("X-Content-Type-Options", "nosniff");
  const url = new URL(req.url, "http://localhost");
  const path = url.pathname.replace("/api/v1", "");
  const json = (value, status = 200) => {
    res.writeHead(status, { "Content-Type": "application/json" });
    res.end(JSON.stringify(value));
  };
  if (url.pathname.startsWith("/api/")) {
    if (path === "/auth/login") {
      authorized = true;
      expire = false;
      return json({ user });
    }
    if (path === "/auth/logout") {
      authorized = false;
      res.writeHead(204);
      return res.end();
    }
    if (!authorized || expire)
      return json({ code: "unauthorized", error: "登录已过期" }, 401);
    if (path === "/auth/session") return json({ user });
    if (path === "/probes/events") {
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.write(
        `event: probes\ndata: ${JSON.stringify({ items: [fixtureProbe] })}\n\n`,
      );
      return;
    }
    let raw = "";
    for await (const chunk of req) raw += chunk;
    const body = raw ? JSON.parse(raw) : {};
    if (path === "/orders" && req.method === "POST") {
      createdKeys.push(body.idempotency_key);
      if (createdKeys.length === 1)
        return json({ code: "payment_uncertain", error: "创建待核实" }, 409);
      return json(uncertainOrder);
    }
    if (path === "/orders/uncertain-fixture/reconcile") {
      reconcileCalls++;
      return json({ code: "unsupported", error: "查询不支持" }, 422);
    }
    if (path === "/rules" && req.method === "POST") {
      rules.push({ ...body, id: "r1", version: 1 });
      return json(rules.at(-1));
    }
    if (path === "/rules/r1" && req.method === "DELETE") {
      rules = [];
      res.writeHead(204);
      return res.end();
    }
    if (path === "/purchases") {
      purchases++;
      return json({ ok: true });
    }
    if (path === "/wallet")
      return json({ currency: "CNY", balance_cents: "9007199254740993123" });
    if (path === "/entitlement") return json(null);
    if (path === "/probes") return json({ items: [fixtureProbe] });
    const maps = {
      "/rules": rules,
      "/nodes": [node],
      "/groups": [group],
      "/users": [user],
      "/audit": [],
      "/plans": [
        {
          id: "p1",
          name: "Fixture plan",
          price_cents: "1000",
          quota_bytes: "10737418240",
          months: 1,
        },
      ],
      "/orders": paymentFixture ? [uncertainOrder] : [],
      "/ledger": [],
      "/payment-channels": [
        {
          id: "epay",
          name: "EPay",
          enabled: paymentFixture,
          status: "unconfigured",
          reason: "未配置",
        },
      ],
    };
    if (path in maps)
      return json({
        items: maps[path],
        total: path === "/orders" && paymentFixture ? 41 : maps[path].length,
      });
    return json({ code: "not_found", error: "未实现" }, 404);
  }
  try {
    const filename = url.pathname.startsWith("/assets/")
      ? resolve(assets, url.pathname.slice(8))
      : resolve(assets, "index.html");
    if (
      !filename.startsWith(
        assets + "/".replace("/", process.platform === "win32" ? "\\" : "/"),
      ) &&
      filename !== resolve(assets, "index.html")
    )
      throw Error();
    const content = await readFile(filename);
    const mime = {
      ".html": "text/html",
      ".js": "text/javascript",
      ".css": "text/css",
      ".svg": "image/svg+xml",
      ".woff2": "font/woff2",
      ".txt": "text/plain",
    };
    res.writeHead(200, {
      "Content-Type": mime[extname(filename)] || "application/octet-stream",
    });
    res.end(content);
  } catch {
    res.writeHead(404);
    res.end();
  }
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const base = `http://127.0.0.1:${server.address().port}`;
try {
  for (const [name, engine] of Object.entries({ chromium, firefox, webkit })) {
    authorized = false;
    expire = false;
    rules = [];
    const browser = await engine.launch({ headless: true });
    try {
      const page = await browser.newPage();
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      page.on("console", (m) => {
        if (m.type() === "error") errors.push(m.text());
      });
      await page.goto(base + "/admin");
      await page.getByLabel("用户名", { exact: true }).fill("fixture-admin");
      await page.getByLabel("密码", { exact: true }).fill("fixture-password");
      await page.getByRole("button", { name: "登录控制台" }).click();
      await page
        .getByRole("heading", { name: "你好，fixture-admin" })
        .waitFor();
      for (const width of [320, 390, 768, 1440, 1920]) {
        await page.setViewportSize({ width, height: 900 });
        await page.getByRole("link", { name: "转发规则", exact: true }).click();
        await page
          .getByRole("heading", { name: "转发规则", exact: true })
          .waitFor();
        assert.ok(
          await page.evaluate(
            () => document.documentElement.scrollWidth <= innerWidth,
          ),
          `${name} ${width} overflow`,
        );
        await page.getByRole("button", { name: "新增", exact: true }).click();
        await page.getByLabel("名称", { exact: true }).fill("dirty");
        await page.getByRole("button", { name: "关闭对话框" }).click();
        await page.getByText("尚有未保存内容。再次关闭将放弃修改。").waitFor();
        await page.getByRole("button", { name: "关闭对话框" }).click();
        assert.equal(await page.locator("dialog").count(), 0);
      }
      await page.getByRole("button", { name: "新增", exact: true }).click();
      await page
        .getByLabel("名称", { exact: true })
        .fill("<img src=x onerror=alert(1)>");
      await page.getByLabel("入口服务器").selectOption("n1");
      await page.getByLabel("设备组").selectOption("g1");
      await page.getByLabel("目标地址", { exact: true }).fill("127.0.0.1:8080");
      await page.getByRole("button", { name: "保存", exact: true }).click();
      await page
        .getByText("<img src=x onerror=alert(1)>", { exact: true })
        .waitFor();
      assert.equal(await page.locator("td img").count(), 0);
      await page.getByRole("link", { name: "实时探针", exact: true }).click();
      await page.getByRole("heading", { name: "Fixture node" }).waitFor();
      assert.ok((await page.getByText("未知", { exact: true }).count()) > 0);
      await page.getByRole("link", { name: "套餐与钱包", exact: true }).click();
      await page.getByText("90071992547409931.23", { exact: false }).waitFor();
      assert.equal(
        await page
          .getByRole("button", { name: "充值钱包", exact: true })
          .isDisabled(),
        true,
      );
      await page.getByRole("button", { name: "余额购买", exact: true }).click();
      await page.getByRole("button", { name: "确认提交" }).click();
      await page
        .getByText("购买已由服务端确认，请查看更新后的权益。")
        .waitFor();
      assert.equal(purchases > 0, true);
      paymentFixture = true;
      createdKeys = [];
      reconcileCalls = 0;
      await page.getByRole("button", { name: "刷新状态", exact: true }).click();
      await page
        .getByText("核实中（创建结果待确认）", { exact: true })
        .waitFor();
      const historyLength = await page.evaluate(() => history.length);
      await page
        .getByRole("button", { name: "下一页订单", exact: true })
        .click();
      await page.waitForURL(/orders_page=2/);
      assert.equal(await page.evaluate(() => history.length), historyLength);
      assert.equal(
        await page.getByRole("link", { name: "前往支付 ↗" }).count(),
        0,
      );
      await page
        .getByRole("button", { name: "核实支付状态", exact: true })
        .click();
      await page
        .getByText("该支付渠道不支持主动查单，请联系管理员核实，勿重复付款。", {
          exact: true,
        })
        .waitFor();
      assert.equal(reconcileCalls, 1);
      await page.getByRole("button", { name: "充值钱包", exact: true }).click();
      await page.getByLabel("支付渠道").selectOption("epay");
      await page.getByRole("button", { name: "确认提交", exact: true }).click();
      await page.locator("dialog .error").waitFor();
      assert.equal(await page.getByLabel("充值金额（分）").isDisabled(), true);
      await page.getByRole("button", { name: "确认提交", exact: true }).click();
      await page.locator("dialog").waitFor({ state: "detached" });
      assert.equal(createdKeys.length, 2);
      assert.equal(createdKeys[0], createdKeys[1]);
      paymentFixture = false;
      for (const accent of [
        "blue",
        "teal",
        "violet",
        "magenta",
        "amber",
        "graphite",
      ])
        await page.getByLabel("品牌配色").selectOption(accent);
      await page.getByRole("button", { name: "切换深色主题" }).click();
      await page.reload();
      assert.equal(
        await page.evaluate(() => document.documentElement.dataset.accent),
        "graphite",
      );
      assert.equal(
        await page.evaluate(() => document.documentElement.dataset.theme),
        "dark",
      );
      await page.setViewportSize({ width: 320, height: 700 });
      assert.ok(
        await page
          .locator("#view")
          .evaluate(
            (el) => el.clientHeight > 0 && el.scrollHeight > el.clientHeight,
          ),
        "main content must scroll",
      );
      if (name === "chromium") {
        await mkdir(resolve(import.meta.dirname, "../.gocache"), {
          recursive: true,
        });
        await page.screenshot({
          path: resolve(import.meta.dirname, "../.gocache/webui-mobile.png"),
        });
      }
      expire = true;
      await page.getByRole("link", { name: "转发规则", exact: true }).click();
      await page.getByRole("button", { name: "登录控制台" }).waitFor();
      assert.deepEqual(
        errors.filter(
          (x) =>
            !x.includes("401") &&
            !x.includes("Unauthorized") &&
            !x.includes("409") &&
            !x.includes("422"),
        ),
        [],
      );
      console.log(
        `${name}: contract, CSP, login, 5 viewport widths, dirty dialog, safe text, probe unknowns, exact money, purchase, themes, 401 PASS`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  server.closeAllConnections();
  await new Promise((resolve) => server.close(resolve));
}
