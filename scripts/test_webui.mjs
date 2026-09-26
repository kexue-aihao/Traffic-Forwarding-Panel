// Browser contract/visual regression with an isolated localhost fixture.
// This is not payment, Agent, database, or real-backend integration evidence.
import {
  chromium,
  firefox,
  webkit,
} from "../web/node_modules/@playwright/test/index.mjs";
import { createServer } from "node:http";
import { once } from "node:events";
import { readFile, mkdir } from "node:fs/promises";
import { resolve, extname } from "node:path";
import assert from "node:assert/strict";
const assets = resolve(import.meta.dirname, "../internal/webui/assets");
const apiDocument = JSON.parse(
  await readFile(
    resolve(import.meta.dirname, "../internal/openapi/openapi.json"),
    "utf8",
  ),
);
let authorized = false;
let expire = false;
let rules = [];
let purchases = 0;
let paymentFixture = false;
let createdKeys = [];
let reconcileCalls = 0;
let historyMode = "samples";
let historyRequests = [];
const summerUTC = "2026-07-14T16:20:30.000Z";
const winterUTC = "2026-01-15T16:20:30.000Z";
let tokenExpiry = "";
let probeOperations = [];
const uncertainOrder = {
  id: "uncertain-fixture",
  channel: "epay",
  amount_cents: "1000",
  currency: "CNY",
  status: "pending",
  created_at: summerUTC,
};
const user = {
  id: "u1",
  username: "fixture-admin",
  role: "admin",
  identity_group_id: "ig1",
  disabled: false,
};
const group = {
  id: "g1",
  name: "Fixture group",
  identity_group_ids: ["ig1"],
  blocked_protocols: [],
  multiplier: "1",
  port_min: 10000,
  port_max: 60000,
  version: 1,
};
const node = {
  id: "n1",
  name: "Fixture node",
  group_ids: ["g1"],
  agent_version: "test",
  last_seen: summerUTC,
  desired_version: 1,
  applied_version: 1,
  apply_error: "",
};
const fixtureProbe = {
  node_id: "n1",
  node_name: "Fixture node",
  group_ids: ["g1"],
  location: { country_code: "HK", country_name: "中国香港", city: "Hong Kong" },
  ipv4_location: { country_code: "HK", country_name: "中国香港", city: "Hong Kong" },
  ipv6_location: { country_code: "JP", country_name: "日本", city: "Tokyo" },
  sampled_at: winterUTC,
  cpu_percent: null,
  memory_used: null,
  memory_total: null,
  disk_used: String(1023n * 1024n ** 3n),
  disk_total: String(1024n ** 4n),
  upload_total: String(1024n ** 4n),
  download_total: String(1023n * 1024n ** 3n),
  upload_bps: 1023 * 1024 ** 3,
  download_bps: 1024 ** 4,
  load1: null,
  cpu_model: "Fixture CPU",
  swap_used: "0",
  swap_total: "2147483648",
  uptime_seconds: null,
  public_ips: [
    {
      address: "203.0.113.8",
      family: "ipv4",
      source: "fixture",
      observed_at: summerUTC,
    },
    {
      address: "2001:db8::8",
      family: "ipv6",
      source: "fixture",
      observed_at: summerUTC,
    },
  ],
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
    if (path === "/site")
      return json({
        name: "流量控制台",
        announcement: "",
        registration: "closed",
        captcha: false,
        accent: "blue",
        currency: "CNY",
        minimum_recharge: "1.00",
        maximum_recharge: "1000000.00",
        payments_enabled: true,
        diagnostics_enabled: true,
        diagnostics_per_minute: 6,
        geo_lookup_url: "https://ipwho.is/{ip}",
      });

    if (path === "/openapi.json") return json(apiDocument);

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
    if (path === "/nodes/n1/operations") return json({ items: probeOperations });
    if (path === "/nodes/n1/operation-access") return json({ token: "fixture-operation-token" }, 201);
    if (["/nodes/n1/shell", "/nodes/n1/uninstall"].includes(path)) {
      const operation = { id: `probe-op-${probeOperations.length}`, kind: path.split("/").at(-1), status: "pending" };
      probeOperations.unshift(operation); return json(operation, 201);
    }
    if (/^\/node-operations\/probe-op-\d+\/cancel$/.test(path)) {
      const operation = probeOperations.find(item => item.id === path.split("/")[2]);
      if (operation) operation.status = "cancelled";
      res.writeHead(204); return res.end();
    }

    if (path === "/nodes/enrollment")
      return json({ token: "fixture-enrollment", expires_at: winterUTC });
    // 设备组的固定接入密钥：长期不变，命令里没有会过期的令牌。
    if (/^\/groups\/[^/]+\/join-key$/.test(path))
      return json({ group_id: "g1", join_key: "a".repeat(64) });
    if (path === "/auth/tokens" && req.method === "POST") {
      tokenExpiry = body.expires_at;
      return json({ token: "fixture-api-token" });
    }
    if (path === "/orders" && req.method === "POST") {
      createdKeys.push(body.idempotency_key);
      if (createdKeys.length === 1)
        return json({ code: "payment_uncertain", error: "创建待核实" }, 409);
      return json(uncertainOrder);
    }
    if (path === "/orders/uncertain-fixture/reconcile") {
      reconcileCalls++;
      // 该协议没有查单能力：接口在、功能不在，后端按 500 报。
      return json({ code: "not_implemented", error: "该支付协议未实现此操作" }, 500);
    }
    if (path === "/rules" && req.method === "POST") {
      rules.push({ ...body, id: "r1", version: 1, category: "" });
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
    if (path === "/entitlement")
      return json({
        id: "entitlement1",
        plan_id: "p1",
        version: 1,
        expires_at: winterUTC,
        quota_bytes: "10737418240",
        used_bytes: "0",
      });
    if (path === "/probes") return json({ items: [fixtureProbe] });
    if (path === "/nodes")
      return json({
        items:
          url.searchParams.get("page") === "2"
            ? [{ ...node, id: "n2", name: "Offline fixture node" }]
            : [node],
        total: 2,
      });
    if (/^\/probes\/[^/]+\/history$/.test(path)) {
      historyRequests.push(url);
      if (historyMode === "error")
        return json({ code: "unavailable", error: "历史查询暂不可用" }, 409);
      if (path.includes("/n2/")) return json({ items: [], total: 0 });
      const resolution = url.searchParams.get("resolution");
      // 波形图五分钟一个点，分钟档的桶也按五分钟造：一个桶一分钟，合起来正好
      // 是一个点。继续造一分钟间隔的数据，等于在验一个已经不会出现的形状。
      const step = resolution === "minute" ? 300000 : 3600000;
      const end = Date.parse(url.searchParams.get("to"));
      if (historyMode === "partial") {
        // 五分钟一组里缺了一分钟：这一组必须整点留空。同一组的上行有值，所以
        // 这条验的是「按指标留空」，而不是「压根没有数据」。
        const bucket = Math.floor((end - 60000) / 300000) * 300000;
        const partial = [0, 1].map((offset) => ({
          sampled_at: new Date(bucket + offset * 60000).toISOString(),
          resolution,
          samples: 1,
          cpu_percent: offset === 0 ? 42 : null,
          memory_percent: 50,
          disk_percent: 0,
          load1: null,
          upload_bps: 1024,
          download_bps: 2048,
        }));
        return json({ items: partial, total: partial.length });
      }
      const items = [0, 1, 2, 3, 5, 6].map((offset, index) => ({
        sampled_at: new Date(end - (7 - offset) * step).toISOString(),
        resolution,
        samples: index + 1,
        cpu_percent: [0, 50, null, 25, 60, 80][index],
        memory_percent: 50,
        disk_percent: 0,
        load1: null,
        upload_bps: [0, 1024, 1024 ** 2, 1024 ** 3, 1024 ** 4 - 1, 1024 ** 4][
          index
        ],
        download_bps: 2048 * index,
      }));
      if (historyMode === "hold") {
        server.emit("history-held", () => {
          if (!res.destroyed) json({ items: items.slice(0, 1), total: 1 });
        });
        return;
      }
      return json({ items, total: items.length });
    }
    // 规则分类：批量归类与按分类筛选，形状与真实后端一致。
    if (path === "/rules/category" && req.method === "POST") {
      const ids = new Set(body.ids || []);
      let updated = 0;
      for (const rule of rules) {
        if (!ids.has(rule.id)) continue;
        rule.category = String(body.category || "");
        updated++;
      }
      return json({ updated, category: String(body.category || "") });
    }
    if (path === "/rules" && req.method === "GET") {
      const wanted = url.searchParams.get("category");
      const uncategorized = url.searchParams.get("uncategorized") === "true";
      const items = rules.filter((rule) =>
        wanted ? rule.category === wanted : uncategorized ? !rule.category : true,
      );
      const categories = [
        ...new Set(rules.map((rule) => rule.category).filter(Boolean)),
      ];
      return json({ items, total: items.length, categories });
    }
    if (path === "/auto-renew") return json({ enabled: false });
    const maps = {
      "/rules": rules,
      "/nodes": [node],
      "/groups": [group],
      "/users": [user],
      "/identity-groups": [
        {
          id: "ig1",
          name: "Fixture identity",
          user_count: 1,
          device_group_count: 1,
        },
      ],
      "/audit": [
        {
          id: "audit1",
          user_id: "u1",
          action: "fixture",
          target: "n1",
          created_at: winterUTC,
        },
      ],
      "/auth/tokens": [
        {
          id: "token1",
          name: "fixture-token",
          expires_at: winterUTC,
          scope: "self",
        },
      ],
      "/plans": [
        {
          id: "p1",
          name: "Fixture plan",
          active: true,
          version: 1,
          kind: "period",
          price_cents: "1000",
          quota_bytes: "10737418240",
          months: 1,
        },
      ],
      "/orders": paymentFixture ? [uncertainOrder] : [],
      "/ledger": [
        {
          id: "ledger1",
          amount_cents: "1000",
          balance_cents: "1000",
          kind: "credit",
          reference: "fixture",
          created_at: winterUTC,
        },
      ],
      "/payment-channels": [
        {
          id: "epay",
          name: "EPay",
          enabled: paymentFixture,
          status: "unconfigured",
          reason: "未配置",
          fee_percent: "1.50",
          fee_fixed: "1.00",
          crypto_currency: "USDT",
          rate: "7.20",
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

/**
 * 自绘下拉走**真实点击**：展开那层列表，点其中的选项，再确认值真的变了。
 *
 * 不用 selectOption：它直接改原生 select 的值并派发 change，永远碰不到那层
 * 自己画的 <li>，所以「选项点不中」这类缺陷它一条都发现不了 —— 而这些下拉
 * 在界面上是唯一的入口。选项的 <li> 不可聚焦，按下鼠标时浏览器会把焦点从
 * 原生 select 上移走；组件若在此时收起列表，点击就会落到列表底下的元素上，
 * 表现出来就是「下拉切不了」。
 */
async function pickOption(page, label, value) {
  const select = page.getByLabel(label).first();
  // evaluateAll 不像点击那样自动等待：页面或弹窗还在渲染时会读到空列表。
  await select.waitFor();
  const values = await select
    .locator("option")
    .evaluateAll((options) => options.map((o) => o.value));
  assert.ok(
    values.includes(value),
    `${label} 的选项里没有 ${value}：${values.join(", ")}`,
  );
  assert.notEqual(
    await select.inputValue(),
    value,
    `${label} 当前已经是 ${value}，这一条就测不到切换了`,
  );
  await select.click();
  const list = page.locator(".select-list");
  await list.waitFor();
  await list.locator(".select-option").nth(values.indexOf(value)).click();
  await list.waitFor({ state: "detached" });
  assert.equal(await select.inputValue(), value, `${label} 应当切到 ${value}`);
}
try {
  for (const [name, engine] of Object.entries({ chromium, firefox, webkit })) {
    authorized = false;
    expire = false;
    rules = [];
    node.capabilities = [];
    probeOperations = [];
    historyMode = "samples";
    historyRequests = [];
    const browser = await engine.launch({ headless: true });
    try {
      const page = await browser.newPage({ timezoneId: "America/New_York" });
      const terminalMessages = [];
      await page.routeWebSocket(/\/node-operations\/[^/]+\/terminal$/, socket => {
        socket.onMessage(message => terminalMessages.push(JSON.parse(message.toString())));
        socket.send(JSON.stringify({ type: "output", data: Buffer.from("root@fixture:~# ").toString("base64") }));
      });

      await page.clock.setFixedTime(new Date(summerUTC));
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      page.on("console", (m) => {
        if (m.type() !== "error") return;
        // 支付核对那条用例故意让后端回 500（该协议没有查单能力：接口在、功能不在），
        // 浏览器会把 5xx 记成控制台错误。只放过这一个响应，别的 5xx 照旧算失败。
        const failed = m.location()?.url ?? "";
        if (failed.includes("/orders/") && failed.endsWith("/reconcile")) return;
        errors.push(m.text());
      });
      await page.goto(base + "/admin");
      await page
        .getByLabel("用户名", { exact: true })
        .waitFor({ timeout: 10000 })
        .catch(async (e) => {
          throw new Error(
            `${e.message}\n${await page.locator("body").innerText()}\n${errors.join("\n")}`,
          );
        });
      await page.getByLabel("用户名", { exact: true }).fill("fixture-admin");
      await page.getByLabel("密码", { exact: true }).fill("fixture-password");
      await page.getByRole("button", { name: "登录控制台" }).click();
      await page
        .getByRole("heading", { name: "你好，fixture-admin" })
        .waitFor();
      await page.getByRole("link", { name: "API 列表", exact: true }).click();
      await page.getByRole("heading", { name: "全站 API 列表" }).waitFor();
      await page.getByText(/共 \d+ 个接口/).waitFor();
      const apiViewport = page.viewportSize();
      for (const width of [apiViewport?.width || 1440, 390]) {
        await page.setViewportSize({ width, height: 900 });
        const overlaps = await page.locator(".api-entry").evaluateAll((items) =>
          items.flatMap((item) => {
            const name = item.querySelector(".api-operation-name");
            const summary = item.querySelector(".api-summary");
            if (!name?.textContent?.trim() || !summary?.textContent?.trim())
              return [];
            return name.getBoundingClientRect().bottom >
              summary.getBoundingClientRect().top + 1
              ? [item.getAttribute("data-operation")]
              : [];
          }),
        );
        assert.deepEqual(overlaps, [], `API labels overlap at ${width}px`);
      }
      if (apiViewport) await page.setViewportSize(apiViewport);
      await page
        .getByLabel("搜索接口", { exact: true })
        .fill("identity-groups");
      const identityCreate = page
        .locator(".api-entry")
        .filter({ hasText: "POST" })
        .filter({ hasText: "/api/v1/identity-groups" })
        .first();
      await identityCreate.waitFor();
      await identityCreate.locator(".api-operation").click();
      await page.getByText("认证方式", { exact: true }).waitFor();
      await page.getByRole("heading", { name: /请求体/ }).waitFor();
      await page.getByRole("button", { name: "清除筛选", exact: true }).click();
      assert.equal(
        await page.evaluate(
          () => new Intl.DateTimeFormat().resolvedOptions().timeZone,
        ),
        "America/New_York",
      );
      await page.getByRole("link", { name: "设备组", exact: true }).click();
      const groupRow = page.locator("tr", { hasText: "Fixture group" });
      // 「设备」：这个组里有哪些机器、版本与心跳。服务器页去掉之后这些落在组上。
      await groupRow.getByRole("button", { name: "设备", exact: true }).click();
      await page.getByText("Fixture node", { exact: true }).waitFor();
      // 心跳必须按上海时间渲染，不跟随浏览器所在时区。
      assert.equal(
        await page
          .locator("dialog td[data-label='最后心跳']")
          .first()
          .innerText(),
        "2026-07-15 00:20:30",
        "设备清单要按上海时间给出心跳",
      );
      // 从设备清单直接进节点运维：两个弹窗叠着开。关掉上面那个不能把下面那个的
      // 滚动锁和路由离开守卫一起解掉 —— 这也是弹窗深度改成计数而不是布尔的原因。
      await page
        .getByRole("dialog")
        .getByRole("button", { name: "节点运维" })
        .first()
        .click();
      const operationsDialog = page
        .locator("dialog")
        .filter({ hasText: "节点运维 · Fixture node" });
      await operationsDialog.waitFor();
      assert.equal(
        await page.locator("dialog").count(),
        2,
        "设备清单与节点运维要能同时开着",
      );
      await page.keyboard.press("Escape");
      await operationsDialog.waitFor({ state: "detached" });
      await page
        .locator("dialog")
        .filter({ hasText: "设备 · Fixture group" })
        .waitFor();
      assert.equal(
        await page.locator("dialog").count(),
        1,
        "关掉上面那个，设备清单还在",
      );
      await page
        .locator("dialog")
        .getByRole("button", { name: "关闭", exact: true })
        .click();
      await page.locator("dialog").waitFor({ state: "detached" });
      // 接入设备：给的是这个组的固定接入密钥，不再是 15 分钟的一次性令牌。
      await groupRow.getByRole("button", { name: "接入设备", exact: true }).click();
      await page.locator(".onboard").waitFor();
      assert.match(
        await page.getByLabel("设备接入命令").textContent(),
        /\/download\/agent-install\.sh\) -t /,
        "命令必须指向面板自托管的接入脚本",
      );
      assert.match(
        await page.locator(".onboard").innerText(),
        /长期不变/,
        "固定密钥的命令不该说 15 分钟过期",
      );
      await page
        .locator("dialog")
        .getByRole("button", { name: "关闭", exact: true })
        .click();
      await page.getByRole("link", { name: "操作审计", exact: true }).click();
      await page.getByText("2026-01-16 00:20:30", { exact: true }).waitFor();
      await page.getByRole("link", { name: "账号与 API", exact: true }).click();
      await page.getByText("2026-01-16 00:20:30", { exact: true }).waitFor();
      await page.getByRole("button", { name: "创建 Token" }).click();
      await page
        .getByLabel("Token 名称", { exact: true })
        .fill("Timezone token");
      await page.getByLabel("有效天数", { exact: true }).fill("1");
      await page.getByRole("button", { name: "确认提交", exact: true }).click();
      await page.getByLabel("API Token 密钥").waitFor();
      assert.equal(
        tokenExpiry,
        "2026-07-15T16:20:30.000Z",
        "one token day is 24 absolute hours, independent of local timezone",
      );
      await page.getByRole("button", { name: "已保存，关闭" }).click();
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
        // 退场是有的：close() 之后还要播一个 --duration-state 的动画，元素
        // 才会从 DOM 上摘掉，所以这里等它卸载而不是立刻数。
        await page.getByRole("button", { name: "关闭对话框" }).click();
        await page.locator("dialog").waitFor({ state: "detached" });
        assert.equal(await page.locator("dialog").count(), 0);
      }
      await page.getByRole("button", { name: "新增", exact: true }).click();
      await page
        .getByLabel("名称", { exact: true })
        .fill("<img src=x onerror=alert(1)>");
      // 入口服务器与设备组合成一项，值同时带机器与组。这一组有两台机器，
      // 所以选项在组名后面补上机器名加以区分（组里只有一台时选项就读作组名）。
      await pickOption(page, "入口服务器", "n1::g1");
      assert.deepEqual(
        await page.getByLabel("入口服务器").locator("option").allTextContents(),
        ["选择服务器", "Fixture group · Fixture node", "Fixture group · Offline fixture node"],
      );
      await page.getByLabel("目标地址", { exact: true }).fill("127.0.0.1:8080");
      await page.getByRole("button", { name: "保存", exact: true }).click();
      await page
        .getByText("<img src=x onerror=alert(1)>", { exact: true })
        .waitFor();
      assert.equal(await page.locator("td img").count(), 0);
      // 规则分类：多选当前页的规则归到一个分类下，列表与筛选器都要跟着走。
      const ruleRow = page.locator("tr", { hasText: "<img src=x onerror=alert(1)>" });
      await ruleRow.getByRole("checkbox").check();
      await page.getByLabel("规则分类", { exact: true }).fill("日本线路");
      await page.getByRole("button", { name: "应用分类", exact: true }).click();
      await page.getByText("已把 1 条规则归到「日本线路」。", { exact: false }).waitFor();
      await ruleRow.getByText("日本线路", { exact: true }).waitFor();
      await page.getByLabel("规则分类筛选").selectOption("日本线路");
      await page.waitForURL(/category=/);
      // 等列表按新筛选渲染完再数行：URL 变了不代表请求回来了。
      await page.locator("tbody tr").first().waitFor();
      assert.equal(await page.locator("tbody tr").count(), 1, "按分类筛选应当只剩这一条");
      await page.getByLabel("规则分类筛选").selectOption("__uncategorized__");
      await page.getByText("暂无数据。", { exact: true }).waitFor();
      await page.getByLabel("规则分类筛选").selectOption("");
      await page.locator("tbody tr").first().waitFor();
      // 实时探针是独立窗口：导航项新开标签页，外壳里不再有嵌入式版本。
      {
        const probeLink = page.getByRole("link", { name: "实时探针", exact: true });
        assert.equal(await probeLink.getAttribute("target"), "_blank", "实时探针应当新开标签页");
        assert.equal(await probeLink.getAttribute("href"), "#/probes");
      }
      // 点导航项要真的开出一个新标签页，且新标签页里是独立布局、会话仍然有效。
      {
        const [tab] = await Promise.all([
          page.waitForEvent("popup"),
          page.getByRole("link", { name: "实时探针", exact: true }).click(),
        ]);
        await tab.waitForLoadState("domcontentloaded");
        assert.equal(await tab.locator(".sidebar").count(), 0, "新标签页里不该有侧栏");
        await tab.getByRole("heading", { name: "Fixture node" }).waitFor();
        await tab.close();
      }
      await page.evaluate(() => { location.hash = "#/probes"; });
      assert.equal(await page.locator(".sidebar").count(), 0, "独立窗口里不该有侧栏");
      await page.getByRole("heading", { name: "Fixture node" }).waitFor();
      await page.locator(".probe-details summary").first().click();
      await page
        .getByText("采样于 2026-01-16 00:20:30", { exact: true })
        .waitFor();
      // 位置图标与设备组归属：位置对普通用户也可见，机器地址不是。
      assert.equal(
        await page
          .locator(".probe-location-cell")
          .nth(0)
          .locator(".probe-address > .location-flag + .probe-ip")
          .count(),
        1,
        "国旗要贴在 IPv4 地址前面、和地址同一行",
      );
      assert.equal(
        await page
          .locator(".probe-location-cell")
          .nth(0)
          .locator(".location-flag")
          .getAttribute("aria-label"),
        "中国香港",
      );
      assert.equal(
        await page
          .locator(".probe-location-cell")
          .nth(1)
          .locator(".location-flag")
          .getAttribute("aria-label"),
        "日本",
      );
      assert.equal(
        await page.locator(".probe-location-cell").nth(0).locator(".probe-label").innerText(),
        "IPv4 地址",
      );
      assert.equal(
        await page.locator(".probe-location-cell").nth(0).locator(".probe-ip").innerText(),
        "203.0.113.8",
      );
      assert.equal(
        await page.locator(".probe-location-cell").nth(1).locator(".probe-label").innerText(),
        "IPv6 地址",
      );
      assert.equal(
        await page.locator(".probe-location-cell").nth(1).locator(".probe-ip").innerText(),
        "2001:db8::8",
      );

      // 完整 IPv6 有 38 个字符，比这一列宽得多。折行可以，横向截断不行 ——
      // 截掉的地址认不出是哪一台，而地址就是这一列的全部内容。
      {
        const ipv6 = page.locator(".probe-location-cell").nth(1).locator(".probe-ip");
        const original = await ipv6.innerText();
        await ipv6.evaluate((el) => { el.textContent = "2406:da14:158:2f00:1539:96b4:591e:777c"; });
        assert.equal(
          await ipv6.evaluate((el) => el.scrollWidth > el.clientWidth),
          false,
          "完整 IPv6 必须折行显示，不能横向截断",
        );
        await ipv6.evaluate((el, text) => { el.textContent = text; }, original);
      }

      // 国旗要和状态标记一样高：两块挨着看，大小不一致会显得没对齐。
      const markBox = await page.locator(".probe-status-square .icon").first().boundingBox();
      const flagBox = await page
        .locator(".probe-location-cell")
        .nth(0)
        .locator(".probe-address .location-flag svg")
        .boundingBox();
      assert.equal(
        Math.round(flagBox.height),
        Math.round(markBox.height),
        "国旗应当和状态标记一样高",
      );

      // 视角开关：切到用户视角，机器地址换成「已隐藏」，位置图标照旧可见。
      await pickOption(page, "视角", "user");
      assert.equal(
        await page.locator(".probe-location-cell").nth(0).locator(".probe-ip").innerText(),
        "已隐藏",
      );
      assert.equal(
        await page.locator(".probe-location-cell").nth(0).locator(".location-flag").count(),
        1,
        "用户视角仍然看得到位置图标",
      );
      // 用户视角看不到机器地址，历史节点改用设备组名认机器：这一组下有两台，
      // 所以带序号区分 —— 只写组名的话两条选项读起来一模一样。
      assert.deepEqual(
        await page.getByLabel("历史节点").locator("option").allTextContents(),
        ["Fixture group-1", "Fixture group-2"],
      );
      // 能不能对机器动手也是用户视角的一部分：WebSSH 与卸载在用户视角下不存在，
      // 否则管理员预览到的不是用户真正看到的那一屏。
      assert.equal(await page.getByRole("button", { name: "WebSSH" }).count(), 0);
      assert.equal(
        await page.getByRole("button", { name: "卸载设备", exact: true }).count(),
        0,
      );
      await pickOption(page, "视角", "admin");
      assert.equal(
        await page.locator(".probe-location-cell").nth(0).locator(".probe-ip").innerText(),
        "203.0.113.8",
      );
      // 管理员看得到地址，就按地址叫它；没上报地址的那台退回机器名。
      assert.deepEqual(
        await page.getByLabel("历史节点").locator("option").allTextContents(),
        ["203.0.113.8", "Offline fixture node"],
      );
      // 切回管理员，操作按钮跟着回来。
      await page.getByRole("button", { name: "WebSSH" }).first().waitFor();
      assert.equal(
        await page.locator(".probe-place").first().innerText(),
        "位置 中国香港·Hong Kong · 设备组 Fixture group",
      );
      // 状态标：在线绿底勾、离线红底叉。换设备组会让页面重新拉一次
      // /probes，借这个真实动作把三种状态各验一遍。
      const squareMark = () =>
        page.locator(".probe-status-square use").first().getAttribute("href");
      fixtureProbe.online = false;
      await pickOption(page, "设备组", "g1");
      await page.locator('.probe-status-square[data-state="offline"]').waitFor();
      assert.equal(await squareMark(), "/assets/icons.svg#x", "离线应当是红底叉");
      fixtureProbe.online = undefined;
      fixtureProbe.sampled_at = new Date().toISOString();
      await pickOption(page, "设备组", "");
      await page.locator('.probe-status-square[data-state="online"]').waitFor();
      assert.equal(await squareMark(), "/assets/icons.svg#check", "在线应当是绿底勾");
      fixtureProbe.sampled_at = winterUTC;
      await pickOption(page, "设备组", "g1");
      await page.locator('.probe-status-square[data-state="stale"]').waitFor();
      assert.equal(await squareMark(), "/assets/icons.svg#clock", "数据陈旧应当是黄底钟");
      await pickOption(page, "设备组", "");
      // 探针页面按设备组收窄：选了组之后仍然只显示这一组的机器。这里走真实
      // 点击：切换是这套自绘下拉唯一的入口，值得按用户的方式验一遍。
      await pickOption(page, "设备组", "g1");
      await page.getByRole("heading", { name: "Fixture node" }).waitFor();
      assert.equal(
        await page.locator(".probe-api code").count(),
        4,
        "设备地址接口应当在页面里写明",
      );
      await page.locator(".probe-details summary").first().evaluate(el => { el.parentElement.open = true; });
      assert.equal(
        await page.getByText("fixture · 2026-07-15 00:20:30", { exact: true }).count(),
        2,
        "dual stack observations should both remain visible in device details",
      );
      assert.ok((await page.getByText("未知", { exact: true }).count()) > 0);
      await page
        .locator(".probe .metrics")
        .getByText("1023 GB/s", { exact: true })
        .waitFor();
      await page
        .locator(".probe .metrics")
        .getByText("1 TB/s", { exact: true })
        .waitFor();
      await page
        .locator(".probe .metrics")
        .getByText("1023 GB / 1 TB", { exact: true })
        .waitFor();
      assert.equal(await page.locator(".probe-row").count(), 1);
      await page.locator(".probe-network").getByText("1 TB", { exact: true }).waitFor();
      assert.equal(await page.locator(".probe-meter meter").count(), 1, "unknown metrics must not become zero meters");
      if (new URL(page.url()).pathname.startsWith("/admin")) {
        await page.getByRole("button", { name: "WebSSH", exact: true }).click();
        await page.getByText("此 Agent 尚不支持该功能。", { exact: false }).waitFor();
        assert.equal(await page.getByRole("button", { name: "连接", exact: true }).count(), 0);
        await page.getByRole("button", { name: "关闭对话框" }).click();
        await page.locator("dialog").waitFor({ state: "detached" });
        await page.getByRole("button", { name: "卸载设备", exact: true }).click();
        await page.locator("dialog[open]").getByText("卸载开始后不能撤销。", { exact: false }).waitFor();
        // Agent 这边还不支持远程卸载时，至少要给出机器上手动卸载的那条命令。
        await page.locator("dialog[open]").getByText("/download/agent-uninstall.sh", { exact: false }).waitFor();
        await page.getByRole("button", { name: "关闭对话框" }).click();
        await page.locator("dialog").waitFor({ state: "detached" });
      }
      node.capabilities = ["shell-v1", "uninstall-v1"];
      const nodesRefreshed = page.waitForResponse(response => response.url().includes("/api/v1/nodes?") && response.status() === 200);
      await page.getByRole("button", { name: "重新连接", exact: true }).click();
      await nodesRefreshed;
      await page.getByRole("button", { name: "WebSSH", exact: true }).click();
      // WebSSH 不问密码：终端直接可开，密码框不该出现。
      assert.equal(await page.getByLabel("管理员密码", { exact: true }).count(), 0, "WebSSH 不该再要管理员密码");
      await page.getByRole("button", { name: "连接", exact: true }).click();
      await page.getByRole("button", { name: "断开连接", exact: true }).waitFor().catch(async e => { throw new Error(`${e.message}\n${await page.getByRole("dialog").innerText()}\n${errors.join("\n")}`); });
      await page.locator(".xterm-helper-textarea").press("a");
      await page.locator(".xterm-helper-textarea").press("Control+c");
      await page.waitForFunction(() => document.querySelector(".xterm") !== null);
      await page.getByRole("button", { name: "断开连接", exact: true }).click();
      await page.getByRole("button", { name: "连接", exact: true }).waitFor();
      assert.ok(terminalMessages.some(message => message.type === "resize" && message.cols > 2));
      assert.ok(terminalMessages.some(message => message.type === "input" && message.data === "a"));
      assert.ok(terminalMessages.some(message => message.type === "input" && message.data === "\x03"));
      await page.getByRole("button", { name: "关闭对话框" }).click();
      await page.locator("dialog").waitFor({ state: "detached" });
      await page.getByRole("button", { name: "卸载设备", exact: true }).click();
      await page.getByLabel("管理员密码", { exact: true }).fill("fixture-password");
      await page.getByRole("button", { name: "确认卸载此设备", exact: true }).click();
      await page.getByText("等待设备接收", { exact: true }).waitFor();
      assert.equal(await page.locator(".probe-row").count(), 1, "pending uninstall cannot hide the device");
      await page.getByRole("button", { name: "取消等待", exact: true }).click();
      await page.getByText("已取消", { exact: true }).waitFor();
      await page.getByRole("button", { name: "关闭对话框" }).click();
      await page.locator("dialog").waitFor({ state: "detached" });
      if (name === "chromium") {
        await mkdir(resolve(import.meta.dirname, "../.gocache/screens"), { recursive: true });
        const viewport = page.viewportSize();
        for (const width of [1440, 390]) {
          await page.setViewportSize({ width, height: 1000 });
          await page.locator(".probe-details").evaluate(el => { el.open = false; });
          await page.evaluate(() => window.scrollTo(0, 0));
          await page.screenshot({ path: resolve(import.meta.dirname, `../.gocache/screens/probe-list-${width}.png`) });
          assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "probe page overflows viewport");
        }
        await page.setViewportSize(viewport);
      }
      const history = page.getByRole("region", { name: "历史趋势" });
      await history.getByText("6 个采样桶 · 5 个CPU有效值").waitFor();
      assert.match(
        await history.locator(".history-window").innerText(),
        /2026-07-14 00:21:00 至 2026-07-15 00:21:00 · 上海时间 UTC\+8/,
      );
      assert.match(
        await history.locator(".history-selection").innerText(),
        /2026-07-15 00:16:00/,
      );
      assert.deepEqual(
        await history.locator(".history-x span").allTextContents(),
        ["00:21", "00:21"],
      );
      assert.equal(
        await history.locator("polyline").count(),
        2,
        "null and absent buckets both break the line",
      );
      assert.equal(
        await page.getByLabel("历史节点").locator("option").count(),
        2,
        "history includes paginated and offline nodes",
      );
      const navigationLength = await page.evaluate(() => window.history.length);
      await pickOption(page, "历史指标", "disk_percent");
      await history
        .locator(".history-selection")
        .getByText("0 %", { exact: true })
        .waitFor();
      // 上下行合并成一条曲线之后不再有「1 分钟负载」这个指标位，选项里也不该
      // 再有它；留下的四个指标就是面板会画的全部。
      assert.deepEqual(
        await page.getByLabel("历史指标").locator("option").allTextContents(),
        ["CPU (%)", "内存 (%)", "磁盘 (%)", "上行/下行 (MB/s)"],
      );
      // 合并之前发出去的链接带的是 upload_bps：改指标不该让旧链接变成空白页，
      // 它落到合并后的那条曲线上。
      await page.evaluate(() => {
        location.replace("#/probes?metric=upload_bps");
      });
      await history.getByText("6 个采样桶 · 6 个上行/下行有效值").waitFor();
      assert.equal(await page.getByLabel("历史指标").inputValue(), "traffic");
      await page.getByLabel("历史指标").selectOption("traffic");
      const slider = page.getByRole("slider", { name: "查看历史采样" });
      await slider.focus();
      await slider.press("Home");
      // 单位固定为 MB/s：1 TB/s 的采样点读作 1048576 MB/s，而不是自适应成
      // 「1 TB/s」—— 两条曲线共用一条纵轴，单位不能随数值跳。
      const readings = [
        "上行 0 MB/s · 下行 0 MB/s",
        "上行 0.001 MB/s · 下行 0.002 MB/s",
        "上行 1 MB/s · 下行 0.004 MB/s",
        "上行 1024 MB/s · 下行 0.006 MB/s",
        "上行 1048576 MB/s · 下行 0.008 MB/s",
        "上行 1048576 MB/s · 下行 0.01 MB/s",
      ];
      for (const [index, expected] of readings.entries()) {
        if (index > 0) await slider.press("ArrowRight");
        await history
          .locator(".history-selection")
          .getByText(expected, { exact: true })
          .waitFor();
      }
      assert.equal(
        await history.locator(".history-y span").first().innerText(),
        "1048576 MB/s",
      );
      if (name === "chromium") {
        await mkdir(resolve(import.meta.dirname, "../.gocache/screens"), {
          recursive: true,
        });
        for (const [size, width] of [
          ["desktop", 1440],
          ["mobile", 320],
        ]) {
          await page.setViewportSize({ width, height: 1000 });
          await page.screenshot({
            path: resolve(
              import.meta.dirname,
              `../.gocache/screens/probe-units-${size}.png`,
            ),
            fullPage: true,
          });
          assert.ok(
            await page.evaluate(
              () => document.documentElement.scrollWidth <= innerWidth,
            ),
            `probe unit labels overflow at ${width}px`,
          );
        }
        await page.setViewportSize({ width: 1440, height: 1000 });
      }
      await page.getByLabel("历史时间范围").selectOption("180d");
      await page.waitForURL(/range=180d/);
      await history.getByText("6 个采样桶 · 6 个上行/下行有效值").waitFor();
      assert.equal(
        historyRequests.at(-1).searchParams.get("resolution"),
        "hour",
      );
      assert.equal(
        historyRequests.at(-1).searchParams.get("to"),
        "2026-07-14T17:00:00.000Z",
        "query boundary stays RFC3339 UTC",
      );
      assert.deepEqual(
        await history.locator(".history-x span").allTextContents(),
        ["2026-01-16", "2026-07-15"],
      );
      assert.equal(
        Date.parse(historyRequests.at(-1).searchParams.get("to")) -
          Date.parse(historyRequests.at(-1).searchParams.get("from")),
        180 * 86400000,
      );
      assert.equal(
        await page.evaluate(() => window.history.length),
        navigationLength,
        "history filters replace the URL",
      );
      await page.reload();
      await history.getByText("6 个采样桶 · 6 个上行/下行有效值").waitFor();
      assert.equal(await page.getByLabel("历史时间范围").inputValue(), "180d");
      assert.equal(
        await page.getByLabel("历史指标").inputValue(),
        "traffic",
      );
      await page.getByLabel("历史节点").selectOption("n2");
      await history.getByText("此时间范围暂无历史采样。").waitFor();
      assert.equal(await history.locator("svg").count(), 0);
      historyMode = "error";
      await page.getByLabel("历史节点").selectOption("n1");
      await history
        .getByRole("alert")
        .filter({ hasText: "历史查询暂不可用" })
        .waitFor();
      historyMode = "samples";
      await history.getByRole("button", { name: "重试历史查询" }).click();
      await history.getByText("6 个采样桶 · 6 个上行/下行有效值").waitFor();
      historyMode = "hold";
      // Wait for the fixture to hold the response before changing its mode.
      const heldRequest = once(server, "history-held", {
        signal: AbortSignal.timeout(15000),
      });
      await page.getByLabel("历史时间范围").selectOption("1h");
      const [releaseHistory] = await heldRequest;
      await history.getByText("正在加载历史采样…").waitFor();
      historyMode = "samples";
      const cancelledHistory = page.waitForEvent("requestfailed", {
        predicate: (request) => request.url().includes("/history?"),
      });
      await page.getByLabel("历史时间范围").selectOption("7d");
      await history.getByText("6 个采样桶 · 6 个上行/下行有效值").waitFor();
      await cancelledHistory;
      releaseHistory();
      await page.waitForTimeout(100);
      await history.getByText("6 个采样桶 · 6 个上行/下行有效值").waitFor();
      assert.equal(
        await page.getByLabel("历史时间范围").inputValue(),
        "7d",
        "late response cannot replace a newer range",
      );
      for (const width of [320, 390, 768, 1440, 1920]) {
        await page.setViewportSize({ width, height: 900 });
        assert.ok(
          await page.evaluate(
            () => document.documentElement.scrollWidth <= innerWidth,
          ),
          `history ${name} ${width} overflow`,
        );
      }
      // 五分钟一组里只要缺一分钟，这个点就整点留空 —— 断线是图上「这里没测到」
      // 的唯一信号，不能被平均抹平。
      historyMode = "partial";
      await page.getByLabel("历史时间范围").selectOption("24h");
      await history.getByText("1 个采样桶 · 1 个上行/下行有效值").waitFor();
      await pickOption(page, "历史指标", "cpu_percent");
      await history.getByText("该指标在此时间范围没有有效值。").waitFor();
      assert.equal(
        await history.locator("svg").count(),
        0,
        "unknown metrics have no synthetic zero chart",
      );
      await pickOption(page, "历史指标", "traffic");
      historyMode = "samples";
      if (name === "chromium") {
        await page.getByLabel("历史时间范围").selectOption("1h");
        await page.getByLabel("历史指标").selectOption("cpu_percent");
        await history.getByText("6 个采样桶 · 5 个CPU有效值").waitFor();
        await mkdir(resolve(import.meta.dirname, "../.gocache/screens"), {
          recursive: true,
        });
        await page.setViewportSize({ width: 1440, height: 1000 });
        await page.screenshot({
          path: resolve(
            import.meta.dirname,
            "../.gocache/screens/history-desktop.png",
          ),
        });
        await page.setViewportSize({ width: 320, height: 900 });
        await page.screenshot({
          path: resolve(
            import.meta.dirname,
            "../.gocache/screens/history-mobile.png",
          ),
        });
        await history.locator(".history-chart").scrollIntoViewIfNeeded();
        await page.screenshot({
          path: resolve(
            import.meta.dirname,
            "../.gocache/screens/history-mobile-chart.png",
          ),
        });
      }
      // 探针是独立窗口，没有侧栏；下面几条用例要回外壳里点导航。
      await page.evaluate(() => { location.hash = "#/overview"; });
      await page.getByRole("link", { name: "套餐与钱包", exact: true }).click();
      await page
        .getByText("到期 2026-01-16 00:20:30", { exact: true })
        .waitFor();
      await page.getByText("2026-01-16 00:20:30", { exact: true }).waitFor();
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
      await page.getByText("2026-07-15 00:20:30", { exact: true }).waitFor();
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
      await pickOption(page, "支付渠道", "epay");
      // 充值是元，手续费与加密报价由服务端配置算出来，界面上先说清楚付多少。
      assert.deepEqual(
        await page.locator("dialog .metrics div").allInnerTexts(),
        [
          "钱包到账\n¥ 100.00",
          "通道手续费\n¥ 2.50 （1.50% + ¥1.00）",
          "实付\n¥ 102.50",
          "折合应付\n≈ 14.24 USDT （汇率 7.20）",
        ],
        "充值报价必须把到账、手续费、实付与折算金额分开写清楚",
      );
      await page.getByRole("button", { name: "确认提交", exact: true }).click();
      await page.locator("dialog .error").waitFor();
      assert.equal(await page.getByLabel("充值金额（元）").isDisabled(), true);
      await page.getByRole("button", { name: "确认提交", exact: true }).click();
      await page.locator("dialog").waitFor({ state: "detached" });
      assert.equal(createdKeys.length, 2);
      assert.equal(createdKeys[0], createdKeys[1]);
      paymentFixture = false;
      // 网络诊断的方式下拉 —— 用户报的就是这一处「选不了 ping / tcping / mtr」。
      // 走真实点击：展开自绘列表、点其中的选项。
      await page.getByRole("link", { name: "网络诊断", exact: true }).click();
      await pickOption(page, "诊断方式", "tcping");
      assert.equal(
        await page.getByLabel("诊断目标").getAttribute("placeholder"),
        "10.20.0.11:27015",
        "切到 tcping 之后，目标提示要跟着变成 主机:端口",
      );
      await pickOption(page, "诊断方式", "mtr");
      await pickOption(page, "诊断方式", "ping");
      // 收尾回到套餐与钱包：下面还有一条以「内容够长」为前提的断言（移动端要能
      // 滚起来），停在内容更短的诊断页会让那条断言看运气 —— WebKit 上就不滚。
      await page.getByRole("link", { name: "套餐与钱包", exact: true }).click();
      await page
        .getByRole("heading", { name: "套餐与钱包", exact: true })
        .waitFor();
      for (const accent of [
        "teal",
        "violet",
        "magenta",
        "amber",
        "blue",
        "graphite",
      ])
        await pickOption(page, "品牌配色", accent);
      // 默认暗色：这套设计语言的使用场景就是近黑画布 + 环境光
      assert.equal(
        await page.evaluate(() => document.documentElement.dataset.theme),
        "dark",
        "default theme must be dark",
      );
      // 明暗是一个轴，品牌色相是另一个轴：切换主题不得扰动配色
      await page.getByRole("button", { name: "切换浅色主题" }).click();
      assert.equal(
        await page.evaluate(() => document.documentElement.dataset.theme),
        "light",
      );
      assert.equal(
        await page.evaluate(() => document.documentElement.dataset.accent),
        "graphite",
        "theme switch must not disturb the accent",
      );
      // 画布底色要跟着主题走，否则移动端地址栏会留着上一套颜色
      assert.equal(
        await page.evaluate(() =>
          document
            .querySelector('meta[name="theme-color"]')
            .getAttribute("content"),
        ),
        "#eceff5",
        "theme-color must follow the light canvas",
      );
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
      assert.equal(
        await page.evaluate(() =>
          document
            .querySelector('meta[name="theme-color"]')
            .getAttribute("content"),
        ),
        "#07080b",
        "防闪烁脚本必须在首帧前把 theme-color 涂成暗色",
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
        `${name}: contract, CSP, login, 5 viewport widths, Shanghai display under America/New_York (summer/winter, UTC rollover, history axes, tokens), dirty dialog, safe text, probe history (gaps/null/zero, keyboard, ranges, offline nodes, retry, stale response), exact money, recharge in yuan with channel fee, purchase, dropdown list clicks (page, dialog, diagnostics), themes, 401 PASS`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  server.closeAllConnections();
  await new Promise((resolve) => server.close(resolve));
}
