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
import { DatabaseSync } from "node:sqlite";
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
// 自绘下拉走真实点击：展开那层列表再点选项。selectOption 直接改原生 select
// 的值，碰不到那层 <li>，测不出「选项点不中、下拉切不了」。
async function pickOption(page, label, value) {
  const select = page.getByLabel(label).first();
  // evaluateAll 不像点击那样自动等待：页面或弹窗还在渲染时会读到空列表。
  await select.waitFor();
  const values = await select
    .locator("option")
    .evaluateAll((options) => options.map((o) => o.value));
  assert.ok(values.includes(value), `${label} 没有 ${value} 这个选项`);
  assert.notEqual(await select.inputValue(), value, `${label} 已经是 ${value}`);
  await select.click();
  const list = page.locator(".select-list");
  await list.waitFor();
  await list.locator(".select-option").nth(values.indexOf(value)).click();
  await list.waitFor({ state: "detached" });
  assert.equal(await select.inputValue(), value, `${label} 应当切到 ${value}`);
}
// Explicit persisted aggregation fixtures test the real history API and UI.
// They do not claim that a real Agent collected these samples or test rollup jobs.
function seedHistory(nodeID) {
  const db = new DatabaseSync(database);
  try {
    db.exec("PRAGMA busy_timeout = 5000");
    const insert = db.prepare(
      "INSERT INTO cp_probe_history(node_id,resolution,bucket,payload) VALUES(?,?,?,?)",
    );
    const minute = Math.floor(Date.now() / 60000) * 60;
    for (const [index, cpu] of [40, 80, null, 0].entries()) {
      insert.run(
        nodeID,
        "minute",
        minute - (10 - index) * 60,
        JSON.stringify({
          samples: 2,
          metrics: [cpu, 50, null, null, 0, 1024].map((metric) => ({
            sum: metric === null ? 0 : metric * 2,
            count: metric === null ? 0 : 2,
          })),
        }),
      );
    }
    insert.run(
      nodeID,
      "hour",
      Math.floor(minute / 3600) * 3600 - 3600,
      JSON.stringify({
        samples: 8,
        metrics: [
          { sum: 240, count: 6 },
          { sum: 400, count: 8 },
          { sum: 0, count: 0 },
          { sum: 0, count: 0 },
          { sum: 0, count: 8 },
          { sum: 8192, count: 8 },
        ],
      }),
    );
  } finally {
    db.close();
  }
}
const historyNodeIDs = [];
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
      await admin.getByRole("link", { name: "API 列表", exact: true }).click();
      await admin.getByRole("heading", { name: "全站 API 列表" }).waitFor();
      await admin.getByText(/共 \d+ 个接口/).waitFor();
      await admin
        .getByLabel("搜索接口", { exact: true })
        .fill("identity-groups");
      const identityAPI = admin
        .locator(".api-entry")
        .filter({ hasText: "POST" })
        .filter({ hasText: "/api/v1/identity-groups" })
        .first();
      await identityAPI.waitFor();
      await identityAPI.locator(".api-operation").click();
      await admin.getByRole("heading", { name: /请求体/ }).waitFor();
      await admin
        .getByRole("button", { name: "清除筛选", exact: true })
        .click();
      const username = `ui-${browserName}`;
      await admin
        .getByRole("link", { name: "身份用户组", exact: true })
        .click();
      for (const name of [`identity-${browserName}`, `spare-${browserName}`]) {
        await admin.getByRole("button", { name: "新增", exact: true }).click();
        await admin.getByLabel("名称", { exact: true }).fill(name);
        await admin
          .getByLabel("身份用户组 ID", { exact: true })
          .fill(`manual-${name}`);
        await save(admin);
        await admin.getByRole("cell", { name, exact: true }).waitFor();
      }
      const identities = await (
        await admin.request.get(base + "/api/v1/identity-groups?page_size=100")
      ).json();
      const identity = identities.items.find(
        (item) => item.name === `identity-${browserName}`,
      );
      const spareIdentity = identities.items.find(
        (item) => item.name === `spare-${browserName}`,
      );
      assert.ok(
        identity.id && spareIdentity.id && identity.id !== spareIdentity.id,
      );
      assert.equal(identity.id, `manual-identity-${browserName}`);
      if (browserName === "chromium") {
        const screenshots = resolve(root, ".gocache/screens");
        await mkdir(screenshots, { recursive: true });
        for (const [size, width] of [
          ["desktop", 1440],
          ["mobile", 320],
        ]) {
          await admin.setViewportSize({ width, height: 900 });
          await admin.screenshot({
            path: resolve(screenshots, `identity-groups-${size}.png`),
          });
        }
        await admin.setViewportSize({ width: 1440, height: 1000 });
      }
      await admin.getByRole("link", { name: "用户管理", exact: true }).click();
      await admin.getByRole("button", { name: "新增", exact: true }).click();
      await admin.getByLabel("用户名", { exact: true }).fill(username);
      await admin
        .getByLabel("身份用户组 ID", { exact: true })
        .selectOption(identity.id);
      await admin.getByRole("button", { name: "保存", exact: true }).click();
      const initialPassword = admin.getByLabel("初始密码", { exact: true });
      await initialPassword.waitFor();
      let userPassword = await initialPassword.inputValue();
      assert.match(userPassword, /^[A-Za-z0-9]{8}(?:-[A-Za-z0-9]{8}){3}$/);
      await admin
        .locator("dialog")
        .getByRole("button", { name: "关闭", exact: true })
        .click();
      await admin.locator("dialog").waitFor({ state: "detached" });
      const createdUserRow = admin
        .getByRole("row")
        .filter({ hasText: username });
      await createdUserRow
        .getByRole("cell", { name: identity.id, exact: true })
        .waitFor();
      assert.equal(
        await createdUserRow
          .getByRole("button", { name: "网络诊断", exact: true })
          .count(),
        0,
      );
      await createdUserRow
        .getByRole("button", { name: "重置密码", exact: true })
        .click();
      await admin
        .getByRole("button", { name: "确认重置", exact: true })
        .click();
      const resetPassword = admin.getByLabel("重置后的新密码", { exact: true });
      await resetPassword.waitFor();
      const replacement = await resetPassword.inputValue();
      assert.match(replacement, /^[A-Za-z0-9]{8}(?:-[A-Za-z0-9]{8}){3}$/);
      assert.notEqual(replacement, userPassword);
      userPassword = replacement;
      await admin
        .locator("dialog")
        .getByRole("button", { name: "关闭", exact: true })
        .click();
      await admin.locator("dialog").waitFor({ state: "detached" });
      await admin.getByRole("link", { name: "设备组", exact: true }).click();
      await admin.getByRole("button", { name: "新增", exact: true }).click();
      await admin
        .getByLabel("名称", { exact: true })
        .fill(`group-${browserName}`);
      await admin.getByLabel("设备类型", { exact: true }).selectOption("entry");
      await admin
        .getByLabel("入口直出策略", { exact: true })
        .selectOption("allow");
      await admin
        .getByLabel(`${identity.name} · ${identity.id}`, { exact: true })
        .check();
      await save(admin);
      const savedGroups = await (
        await admin.request.get(base + "/api/v1/groups?page_size=100")
      ).json();
      const savedGroup = savedGroups.items.find(
        (group) => group.name === `group-${browserName}`,
      );
      assert.deepEqual(savedGroup.identity_group_ids, [identity.id]);
      assert.equal(savedGroup.user_ids, undefined);
      assert.deepEqual(savedGroup.blocked_protocols, []);
      assert.equal(savedGroup.type, "entry");
      assert.deepEqual(savedGroup.disabled_transports, []);
      assert.deepEqual(savedGroup.disabled_networks, []);
      const groupRow = admin
        .getByRole("row")
        .filter({ hasText: `group-${browserName}` });
      await groupRow
        .getByRole("button", { name: "高级设置", exact: true })
        .click();
      const advanced = admin.getByLabel("设备组高级设置 JSON", { exact: true });
      assert.match(await advanced.inputValue(), /\/\/ 入站屏蔽选项/);
      assert.match(await advanced.inputValue(), /"max_fail": 3/);
      assert.match(await advanced.inputValue(), /"fail_timout_sec": 30/);
      if (browserName === "chromium") {
        const screenshotDir = resolve(root, ".gocache/screens");
        await mkdir(screenshotDir, { recursive: true });
        const viewport = admin.viewportSize();
        for (const [size, width, height] of [
          ["desktop", 1440, 1000],
          ["mobile", 320, 900],
        ]) {
          await admin.setViewportSize({ width, height });
          assert.ok(
            await admin
              .locator("dialog")
              .evaluate((el) => el.scrollWidth <= el.clientWidth),
          );
          await admin.screenshot({
            path: resolve(screenshotDir, `group-advanced-${size}.png`),
          });
          await admin.locator(".advanced-help summary").click();
          await admin
            .getByRole("heading", { name: "反向隧道选项", exact: true })
            .scrollIntoViewIfNeeded();
          assert.ok(
            await admin
              .locator("dialog")
              .evaluate((el) => el.scrollWidth <= el.clientWidth),
          );
          await admin.screenshot({
            path: resolve(screenshotDir, `group-advanced-help-${size}.png`),
          });
          await admin.locator(".advanced-help summary").click();
          await advanced.scrollIntoViewIfNeeded();
          await admin.locator("dialog").evaluate((el) => {
            el.scrollTop = 0;
          });
        }
        await admin.setViewportSize(viewport);
      }
      await advanced.fill("[]");
      await admin
        .getByRole("button", { name: "保存高级设置", exact: true })
        .click();
      await admin
        .getByRole("alert")
        .filter({ hasText: "必须是 JSON 对象" })
        .waitFor();
      await advanced.fill("{} /* missing end");
      await admin
        .getByRole("button", { name: "保存高级设置", exact: true })
        .click();
      await admin
        .getByRole("alert")
        .filter({ hasText: "块注释未结束" })
        .waitFor();
      await advanced.fill(
        '{"allowed_host":["example.com"],"blocked_path":["/private"]}',
      );
      await admin
        .getByRole("button", { name: "保存高级设置", exact: true })
        .click();
      await admin
        .getByRole("alert")
        .filter({ hasText: "allowed_host" })
        .waitFor();
      const extraSettings = {
        blocked_protocol: ["socks"],
        disable_udp: true,
        max_fail: 0,
        fail_timout_sec: 0,
        blocked_path: ["/test//path", "/test/*literal*/"],
        tls: { server_name: "example.com", nested: { enabled: false } },
      };
      await advanced.fill(
        `// extra settings\n${JSON.stringify(extraSettings, null, 2)}\n/* end */`,
      );
      await admin.getByRole("button", { name: "格式化", exact: true }).click();
      await admin
        .getByRole("button", { name: "保存高级设置", exact: true })
        .click();
      await admin.locator("dialog").waitFor({ state: "detached" });
      await admin
        .getByRole("row")
        .filter({ hasText: `group-${browserName}` })
        .waitFor();
      const updatedGroups = await (
        await admin.request.get(base + "/api/v1/groups?page_size=100")
      ).json();
      const updatedGroup = updatedGroups.items.find(
        (group) => group.name === `group-${browserName}`,
      );
      assert.deepEqual(updatedGroup.advanced, extraSettings);
      await admin
        .getByRole("row")
        .filter({ hasText: `group-${browserName}` })
        .getByRole("button", { name: "编辑", exact: true })
        .click();
      const basicDialog = admin.locator("dialog");
      assert.equal(
        await basicDialog.getByText("屏蔽协议", { exact: true }).count(),
        0,
      );
      assert.equal(
        await basicDialog.getByText("允许的转发方式", { exact: true }).count(),
        0,
      );
      if (browserName === "chromium") {
        const screenshotDir = resolve(root, ".gocache/screens");
        await mkdir(screenshotDir, { recursive: true });
        const viewport = admin.viewportSize();
        for (const [size, width, height] of [
          ["desktop", 1440, 1000],
          ["mobile", 320, 900],
        ]) {
          await admin.setViewportSize({ width, height });
          assert.ok(
            await admin.evaluate(
              () => document.documentElement.scrollWidth <= innerWidth,
            ),
          );
          assert.ok(
            await admin
              .locator("dialog")
              .evaluate((el) => el.scrollWidth <= el.clientWidth),
          );
          await admin.screenshot({
            path: resolve(screenshotDir, `group-policies-${size}.png`),
          });
        }
        await admin.setViewportSize(viewport);
      }
      await save(admin);
      await groupRow
        .getByRole("button", { name: "高级设置", exact: true })
        .click();
      assert.match(await advanced.inputValue(), /"max_fail": 0/);
      assert.match(await advanced.inputValue(), /"fail_timout_sec": 0/);
      assert.doesNotMatch(await advanced.inputValue(), /app:socks/);
      await admin
        .getByRole("button", { name: "保存高级设置", exact: true })
        .click();
      await admin.locator("dialog").waitFor({ state: "detached" });
      const reopenedGroups = await (
        await admin.request.get(base + "/api/v1/groups?page_size=100")
      ).json();
      assert.deepEqual(
        reopenedGroups.items.find((group) => group.id === savedGroup.id)
          .advanced,
        extraSettings,
      );
      // 面板托管接入脚本与 Agent 产物，两个路由都必须**免登录**可取 ——
      // 接入命令在目标设备上执行，那里没有会话。
      const installer = await fetch(base + "/download/agent-install.sh");
      assert.equal(installer.status, 200, "接入脚本必须免登录可取");
      assert.match(installer.headers.get("content-type"), /shellscript/);
      const installerBody = await installer.text();
      assert.match(installerBody, /^#!\/usr\/bin\/env bash/);
      assert.match(installerBody, /\/download\/agent\/linux\//);
      const absent = await fetch(base + "/download/agent/plan9/sparc");
      assert.equal(absent.status, 404);
      assert.match(
        await absent.text(),
        /plan9\/sparc/,
        "缺产物时必须说明是哪个平台，而不是回一个空 404",
      );

      await admin.getByRole("link", { name: "服务器", exact: true }).click();
      await admin.getByRole("button", { name: "生成接入凭据" }).click();
      await admin
        .getByLabel("名称", { exact: true })
        .fill(`simulated-${browserName}`);
      await admin.getByLabel(`group-${browserName}`, { exact: true }).check();
      await admin.getByRole("button", { name: "保存", exact: true }).click();
      // 交给运营方的是一条自包含命令，令牌藏在里面。从命令里取令牌，
      // 顺带把命令本身的形状也钉住。
      const onboardCommand = (
        await admin.getByLabel("设备接入命令").textContent()
      ).trim();
      assert.match(
        onboardCommand,
        /^bash <\(curl -fLsS https?:\/\/[^\s]+\/download\/agent-install\.sh\) -t '[0-9a-f]{64}' -u 'https?:\/\/[^']+' -n 'simulated-[^']+'$/,
      );
      const enrollment = onboardCommand.match(/-t '([0-9a-f]{64})'/)[1];
      assert.ok(enrollment.length > 20);
      await admin
        .locator("dialog")
        .getByRole("button", { name: "关闭", exact: true })
        .click();

      // 「接入设备」给的是设备组的**固定**接入密钥：命令里没有会过期的令牌，
      // 同一组的设备共用这一条，装失败可以直接再跑一次。
      //
      // 用一把独立的空组来验证，不往 group-${browserName} 里塞节点 —— 后面
      // 探针视图的断言依赖该组成员看到的历史桶数量，多出节点会把默认选中项挤掉。
      await admin.getByRole("link", { name: "设备组", exact: true }).click();
      await admin.getByRole("button", { name: "新增", exact: true }).click();
      await admin
        .getByLabel("名称", { exact: true })
        .fill(`join-${browserName}`);
      await admin.getByLabel("设备类型", { exact: true }).selectOption("entry");
      await save(admin);
      const joinRow = admin.locator("tr", { hasText: `join-${browserName}` });
      await joinRow.getByRole("button", { name: "接入设备" }).click();
      await admin.locator("dialog").waitFor();
      const groupCommand = (
        await admin.getByLabel("设备接入命令").textContent()
      ).trim();
      assert.match(
        groupCommand,
        /^bash <\(curl -fLsS https?:\/\/[^\s]+\/download\/agent-install\.sh\) -t '[0-9a-f]{64}' -u 'https?:\/\/[^']+'$/,
        "固定密钥的命令不带 -n：设备名由设备自报",
      );
      const groupKey = groupCommand.match(/-t '([0-9a-f]{64})'/)[1];
      await admin.keyboard.press("Escape");
      await admin.locator("dialog").waitFor({ state: "detached" });

      // 关掉再打开必须还是同一把 —— 这正是「固定」的含义，也是一次性令牌
      // 做不到的地方。
      await joinRow.getByRole("button", { name: "接入设备" }).click();
      await admin.locator("dialog").waitFor();
      assert.equal(
        (await admin.getByLabel("设备接入命令").textContent()).trim(),
        groupCommand,
        "重复打开必须给出同一条命令",
      );

      // 用这把密钥真的接入两台设备。本机没有 Linux 目标机，所以走 API 而不是
      // 执行那条 bash 命令 —— 这里验证的是凭据在控制面这一侧确实可用、可重复。
      for (const name of ["key-a-" + browserName, "key-b-" + browserName]) {
        const joined = await fetch(base + "/api/v1/agent/register", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            token: groupKey,
            name,
            agent_version: "test",
            os: "linux",
            arch: "amd64",
          }),
        });
        assert.equal(joined.status, 201, await joined.text());
      }
      await admin.keyboard.press("Escape");
      await admin.locator("dialog").waitFor({ state: "detached" });
      // Delete an idle group with enrolled devices; closing the confirmation
      // leaves it intact, while confirming detaches devices and revokes its key.
      await joinRow.getByRole("button", { name: "删除", exact: true }).click();
      await admin
        .getByRole("heading", { name: "删除设备组", exact: true })
        .waitFor();
      await admin.keyboard.press("Escape");
      await admin.locator("dialog").waitFor({ state: "detached" });
      await joinRow.waitFor();
      await joinRow.getByRole("button", { name: "删除", exact: true }).click();
      if (browserName === "chromium") {
        const viewport = admin.viewportSize();
        for (const [size, width] of [
          ["desktop", 1440],
          ["mobile", 320],
        ]) {
          await admin.setViewportSize({ width, height: 900 });
          assert.ok(
            await admin
              .locator("dialog")
              .evaluate((el) => el.scrollWidth <= el.clientWidth + 1),
          );
          await admin.screenshot({
            path: resolve(root, `.gocache/screens/group-delete-${size}.png`),
          });
        }
        await admin.setViewportSize(viewport);
      }
      await admin
        .getByRole("button", { name: "确认删除", exact: true })
        .click();
      await admin.locator("dialog").waitFor({ state: "detached" });
      await joinRow.waitFor({ state: "detached" });
      const revokedGroupKey = await admin.request.post(
        base + "/api/v1/agent/register",
        {
          data: { token: groupKey, name: "deleted-group" },
        },
      );
      assert.equal(revokedGroupKey.status(), 401);
      const registration = await admin.request.post(
        base + "/api/v1/agent/register",
        {
          data: {
            token: enrollment,
            name: `simulated-${browserName}`,
            agent_version: "ui-test-simulation",
            os: "simulated",
            arch: "simulated",
            capabilities: ["tcp", "direct", "looking-glass-v1"],
          },
        },
      );
      assert.equal(registration.status(), 201);
      const registered = await registration.json();
      seedHistory(registered.node_id);
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
      await admin.getByLabel("账号规则总数", { exact: true }).fill("3");
      await admin.getByLabel("每节点最大连接数", { exact: true }).fill("10");
      await admin.getByLabel("每节点活跃 IP 数", { exact: true }).fill("2");
      await admin
        .getByLabel("每节点上下行合计（B/s）", { exact: true })
        .fill("1048576");
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
      await user.getByRole("link", { name: "API 列表", exact: true }).click();
      await user.getByRole("heading", { name: "全站 API 列表" }).waitFor();
      await user.getByText(/共 \d+ 个接口/).waitFor();
      assert.equal(
        await user
          .getByRole("link", { name: "身份用户组", exact: true })
          .count(),
        0,
      );
      assert.equal(
        (await user.request.get(base + "/api/v1/identity-groups")).status(),
        403,
      );
      await admin
        .getByRole("link", { name: "身份用户组", exact: true })
        .click();
      await admin
        .getByRole("row")
        .filter({ hasText: identity.name })
        .getByRole("button", { name: "编辑", exact: true })
        .click();
      assert.equal(
        await admin.getByLabel("身份用户组 ID", { exact: true }).inputValue(),
        identity.id,
      );
      await admin
        .getByLabel("身份用户组 ID", { exact: true })
        .fill(spareIdentity.id);
      await admin
        .locator("dialog")
        .getByRole("button", { name: "保存", exact: true })
        .click();
      await admin
        .locator("dialog")
        .getByRole("alert")
        .filter({ hasText: "ID 或名称可能已存在" })
        .waitFor();
      const editedIdentityID = `clients-${browserName}`;
      await admin
        .getByLabel("身份用户组 ID", { exact: true })
        .fill(editedIdentityID);
      await admin
        .getByLabel("名称", { exact: true })
        .fill(`${identity.name}-edited`);
      await save(admin);
      identity.id = editedIdentityID;
      identity.name += "-edited";
      await admin
        .getByRole("row")
        .filter({ hasText: identity.name })
        .getByRole("cell", { name: identity.id, exact: true })
        .waitFor();
      const sessionAfterIDEdit = await (
        await user.request.get(base + "/api/v1/auth/session")
      ).json();
      assert.equal(sessionAfterIDEdit.user.identity_group_id, identity.id);
      const visibleAfterIDEdit = await (
        await user.request.get(base + "/api/v1/groups")
      ).json();
      assert.equal(visibleAfterIDEdit.items.length, 1);
      assert.equal(visibleAfterIDEdit.items[0].id, savedGroup.id);
      // Existing sessions must immediately follow the user's current identity ID.
      await admin.getByRole("link", { name: "用户管理", exact: true }).click();
      await admin
        .getByRole("row")
        .filter({ hasText: username })
        .getByRole("cell", { name: identity.id, exact: true })
        .waitFor();
      for (const [nextIdentity, expectedGroups] of [
        [spareIdentity, 0],
        [identity, 1],
      ]) {
        await admin
          .getByRole("row")
          .filter({ hasText: username })
          .getByRole("button", { name: "身份组", exact: true })
          .click();
        assert.equal(
          await admin.getByLabel("角色", { exact: true }).isDisabled(),
          true,
        );
        await admin
          .getByLabel("身份用户组 ID", { exact: true })
          .selectOption(nextIdentity.id);
        await save(admin);
        await admin
          .getByRole("row")
          .filter({ hasText: username })
          .getByRole("cell", { name: nextIdentity.id, exact: true })
          .waitFor();
        const groups = await (
          await user.request.get(base + "/api/v1/groups")
        ).json();
        const nodes = await (
          await user.request.get(base + "/api/v1/nodes")
        ).json();
        assert.equal(groups.items.length, expectedGroups);
        assert.equal(nodes.items.length, expectedGroups);
      }
      await admin
        .getByRole("link", { name: "身份用户组", exact: true })
        .click();
      await admin
        .getByRole("row")
        .filter({ hasText: identity.name })
        .getByRole("button", { name: "删除", exact: true })
        .click();
      await admin
        .locator("dialog")
        .getByRole("button", { name: "确认删除", exact: true })
        .click();
      await admin
        .locator("dialog")
        .getByRole("alert")
        .filter({ hasText: "仍被用户或设备组引用" })
        .waitFor();
      await admin.keyboard.press("Escape");
      await admin.locator("dialog").waitFor({ state: "detached" });
      await admin
        .getByRole("row")
        .filter({ hasText: spareIdentity.name })
        .getByRole("button", { name: "删除", exact: true })
        .click();
      await admin
        .locator("dialog")
        .getByRole("button", { name: "确认删除", exact: true })
        .click();
      await admin.locator("dialog").waitFor({ state: "detached" });
      await admin
        .getByRole("cell", { name: spareIdentity.name, exact: true })
        .waitFor({ state: "detached" });
      assert.equal(
        await user.getByRole("link", { name: "用户管理", exact: true }).count(),
        0,
      );
      assert.equal(
        (await user.request.get(base + "/api/v1/users")).status(),
        403,
      );
      // 探针是付费能力：这个账号此时还没有有效权益，列表与历史都要拒。
      // 内容断言放在下面购买套餐之后。
      await user.getByRole("link", { name: "实时探针", exact: true }).click();
      await user
        .getByText("需要有效的套餐权益才能查看探针", { exact: false })
        .waitFor();
      assert.equal(
        (await user.request.get(base + "/api/v1/probes")).status(),
        403,
      );
      assert.equal(
        (
          await user.request.get(
            base + `/api/v1/probes/${registered.node_id}/history`,
          )
        ).status(),
        403,
      );
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
      await user.getByRole("button", { name: "编辑", exact: true }).click();
      await user.getByLabel("转发方式", { exact: true }).selectOption("tls");
      await user
        .getByLabel("隧道端点", { exact: true })
        .fill("first.example.test:443");
      await user
        .getByLabel("隧道凭据", { exact: true })
        .fill("fixture-chain-first-secret");
      for (const hop of [2, 3]) {
        await user
          .getByRole("button", { name: "添加后续出口", exact: true })
          .click();
        await user
          .getByLabel(`出口 ${hop} 端点`, { exact: true })
          .fill(`exit${hop}.example.test:443`);
        await user
          .getByLabel(`出口 ${hop} 凭据`, { exact: true })
          .fill(`fixture-chain-${hop}-secret`);
      }
      await save(user);
      const chainedRules = await (
        await user.request.get(base + "/api/v1/rules")
      ).json();
      assert.equal(chainedRules.items[0].tunnel.chain.length, 2);
      assert.equal(chainedRules.items[0].tunnel.chain[0].token, undefined);
      await user.getByRole("button", { name: "编辑", exact: true }).click();
      assert.equal(
        await user.getByLabel("出口 2 端点", { exact: true }).inputValue(),
        "exit2.example.test:443",
      );
      assert.equal(
        await user.getByLabel("出口 2 凭据", { exact: true }).inputValue(),
        "",
      );
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
      // Real operation APIs: issue and redeem a code, create/poll a task,
      // and validate user/admin page boundaries on all three browser engines.
      await admin
        .getByRole("link", { name: "运营与任务", exact: true })
        .click();
      await admin.getByLabel("离线宽限（秒）", { exact: true }).fill("120");
      await admin
        .getByRole("button", { name: "保存告警设置", exact: true })
        .click();
      await admin.getByText("告警设置已保存。", { exact: false }).waitFor();
      assert.equal(
        (await (await admin.request.get(base + "/api/v1/alert-policy")).json())
          .offline_seconds,
        120,
      );
      await admin.getByLabel("充值金额（分）", { exact: true }).fill("10000");
      await admin
        .getByRole("button", { name: "生成兑换码", exact: true })
        .click();
      await admin
        .getByRole("heading", { name: "兑换码（仅显示本次）", exact: true })
        .waitFor();
      const redeemCode = (await admin.locator(".mono").innerText()).trim();
      await user.getByRole("link", { name: "运营与任务", exact: true }).click();
      assert.equal(
        await user
          .getByRole("heading", { name: "充值退款", exact: true })
          .count(),
        0,
      );
      await user.getByLabel("兑换码", { exact: true }).fill(redeemCode);
      await user.getByRole("button", { name: "兑换", exact: true }).click();
      await user
        .getByText("兑换成功，请在套餐与钱包页面查看权益。", { exact: false })
        .waitFor();
      const fundedWallet = await (
        await user.request.get(base + "/api/v1/wallet")
      ).json();
      assert.equal(fundedWallet.balance_cents, "10000");
      await user
        .getByLabel("接收地址", { exact: true })
        .fill("https://hooks.example.test/events");
      await user
        .getByRole("button", { name: "添加通知地址", exact: true })
        .click();
      await user
        .getByRole("heading", {
          name: "通知签名密钥（仅显示本次）",
          exact: true,
        })
        .waitFor();
      await user
        .getByRole("button", { name: "已保存，关闭", exact: true })
        .click();
      await user
        .getByRole("button", { name: "静默一小时", exact: true })
        .click();
      await user
        .getByRole("button", { name: "解除静默", exact: true })
        .waitFor();
      const mutedHooks = await (
        await user.request.get(base + "/api/v1/webhooks")
      ).json();
      assert.ok(Date.parse(mutedHooks.items[0].muted_until) > Date.now());
      await user.getByRole("button", { name: "解除静默", exact: true }).click();
      await user
        .getByRole("button", { name: "静默一小时", exact: true })
        .waitFor();
      await user.getByRole("button", { name: "停用通知", exact: true }).click();
      await user
        .getByRole("button", { name: "启用通知", exact: true })
        .waitFor();
      await user.getByRole("button", { name: "启用通知", exact: true }).click();
      await user
        .getByRole("button", { name: "停用通知", exact: true })
        .waitFor();
      await user
        .getByRole("heading", { name: "最近事件", exact: true })
        .waitFor();
      assert.equal(
        await user
          .getByRole("heading", { name: "告警设置", exact: true })
          .count(),
        0,
      );
      await user.getByRole("button", { name: "删除地址", exact: true }).click();
      await user.getByText("暂无通知地址。", { exact: true }).waitFor();
      await user
        .getByRole("button", { name: "创建导出任务", exact: true })
        .click();
      await user
        .getByRole("button", { name: "查看结果", exact: true })
        .first()
        .waitFor();
      const taskList = await (
        await user.request.get(base + "/api/v1/tasks")
      ).json();
      for (let attempt = 0; attempt < 30; attempt++) {
        const task = await (
          await user.request.get(base + "/api/v1/tasks/" + taskList.items[0].id)
        ).json();
        if (task.status === "completed") break;
        if (attempt === 29) throw Error("export task did not complete");
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
      await user
        .getByRole("button", { name: "查看结果", exact: true })
        .first()
        .click();
      await user
        .getByRole("button", { name: "下载 JSON", exact: true })
        .waitFor();
      await user.getByRole("link", { name: "套餐与钱包", exact: true }).click();
      await user
        .getByLabel("自动续费套餐", { exact: true })
        .selectOption({ index: 1 });
      await user.locator(".toggle-row input").check();
      await user.getByText("已开启", { exact: true }).waitFor();
      await user.locator(".toggle-row input").uncheck();
      await user.getByText("已关闭", { exact: true }).waitFor();
      await user
        .getByRole("button", { name: "余额购买", exact: true })
        .first()
        .click();
      await user.getByRole("button", { name: "确认提交", exact: true }).click();
      await user.locator("dialog").waitFor({ state: "detached" });
      const beforeAddon = await (
        await user.request.get(base + "/api/v1/entitlement")
      ).json();
      assert.equal(beforeAddon.limits.max_rules, 3);
      assert.equal(beforeAddon.limits.max_connections_per_node, 10);
      assert.equal(beforeAddon.limits.max_ips_per_node, 2);
      assert.equal(beforeAddon.limits.bytes_per_second_per_node, "1048576");
      // 有了权益，探针才可见：只包含本组设备，且对普通用户隐藏公网 IP。
      await user.getByRole("link", { name: "实时探针", exact: true }).click();
      await user
        .getByRole("heading", { name: `simulated-${browserName}` })
        .waitFor();
      assert.equal(
        (await user.locator("body").innerText()).includes("203.0.113.99"),
        false,
      );
      assert.equal(await user.getByRole("button", { name: "WebSSH", exact: true }).count(), 0);
      assert.equal(await user.getByRole("button", { name: "卸载设备", exact: true }).count(), 0);
      assert.equal(await user.getByLabel("视角", { exact: true }).count(), 0, "普通账号没有视角开关");
      const history = user.getByRole("region", { name: "历史趋势" });
      await history.getByText("4 个采样桶 · 3 个CPU有效值").waitFor();
      await history
        .locator(".history-selection")
        .getByText("0 %", { exact: true })
        .waitFor();
      assert.equal(await history.locator("polyline").count(), 1);
      assert.equal(
        (
          await user.request.get(
            base + `/api/v1/probes/${registered.node_id}/history`,
          )
        ).status(),
        200,
      );
      const historical = await (
        await user.request.get(
          base + `/api/v1/probes/${registered.node_id}/history`,
        )
      ).json();
      assert.equal(historical.total, 4);
      assert.equal(historical.items[0].memory_percent, 50);
      assert.equal(historical.items[0].disk_percent, null);
      assert.equal(JSON.stringify(historical).includes("public_ips"), false);
      assert.equal(JSON.stringify(historical).includes("203.0.113.99"), false);
      for (const forbiddenNode of historyNodeIDs)
        assert.equal(
          (
            await user.request.get(
              base + `/api/v1/probes/${forbiddenNode}/history`,
            )
          ).status(),
          404,
          "history obeys current node membership",
        );
      historyNodeIDs.push(registered.node_id);
      await user.getByLabel("历史时间范围").selectOption("30d");
      await history.getByText("1 个采样桶 · 1 个CPU有效值").waitFor();
      await history
        .locator(".history-selection")
        .getByText("40 %", { exact: true })
        .waitFor();
      await user.reload();
      await history.getByText("1 个采样桶 · 1 个CPU有效值").waitFor();
      assert.equal(await user.getByLabel("历史时间范围").inputValue(), "30d");
      const userNodes = await (
        await user.request.get(base + "/api/v1/nodes")
      ).json();
      assert.equal(userNodes.items.length, 1);

      await admin
        .getByRole("link", { name: "套餐与钱包", exact: true })
        .click();
      await admin
        .getByRole("button", { name: "新增套餐", exact: true })
        .click();
      await admin.getByLabel("套餐类型").selectOption("addon");
      await admin
        .getByLabel("名称", { exact: true })
        .fill(`addon-${browserName}`);
      await admin.getByLabel("价格（分）", { exact: true }).fill("100");
      await admin.getByLabel("流量配额（字节）", { exact: true }).fill("1024");
      await admin
        .getByRole("button", { name: "确认提交", exact: true })
        .click();
      await admin.locator("dialog").waitFor({ state: "detached" });
      const addonCard = admin.locator("article").filter({
        has: admin.getByRole("heading", {
          name: `addon-${browserName}`,
          exact: true,
        }),
      });
      await addonCard
        .getByRole("button", { name: "编辑套餐", exact: true })
        .click();
      await admin.getByLabel("价格（分）", { exact: true }).fill("200");
      await admin
        .getByRole("button", { name: "确认提交", exact: true })
        .click();
      await admin.locator("dialog").waitFor({ state: "detached" });
      // 上面的探针断言把用户留在了探针页，这里回到套餐与钱包 —— 下面几条
      // 断言都在这个页面上。
      await user.getByRole("link", { name: "套餐与钱包", exact: true }).click();
      await user.getByRole("button", { name: "刷新状态", exact: true }).click();
      const userAddon = user.locator("article").filter({
        has: user.getByRole("heading", {
          name: `addon-${browserName}`,
          exact: true,
        }),
      });
      await userAddon
        .getByRole("button", { name: "购买叠加包", exact: true })
        .click();
      await user
        .getByText("叠加包增加当前周期配额，到期时间保持不变。", {
          exact: true,
        })
        .waitFor();
      await user.getByRole("button", { name: "确认提交", exact: true }).click();
      await user.locator("dialog").waitFor({ state: "detached" });
      const afterAddon = await (
        await user.request.get(base + "/api/v1/entitlement")
      ).json();
      assert.equal(afterAddon.expires_at, beforeAddon.expires_at);
      assert.equal(
        BigInt(afterAddon.quota_bytes),
        BigInt(beforeAddon.quota_bytes) + 1024n,
      );
      // Functional completion: real APIs and embedded pages, no external gateways.
      await admin.getByRole("link", { name: "站点设置", exact: true }).click();
      await admin
        .getByLabel("公告", { exact: true })
        .fill(`功能测试 ${browserName}`);
      await admin
        .getByLabel("注册策略", { exact: true })
        .selectOption("invite");
      await admin
        .getByRole("button", { name: "保存站点设置", exact: true })
        .click();
      await admin
        .getByText("站点设置已保存，刷新页面可查看品牌更新。", { exact: false })
        .waitFor();
      await admin
        .getByRole("button", { name: "生成注册邀请码", exact: true })
        .click();
      const siteInvite = await admin
        .getByLabel("请保存邀请码", { exact: true })
        .inputValue();
      const registrationContext = await browser.newContext();
      const visitor = await registrationContext.newPage();
      await visitor.goto(base + "/");
      await visitor
        .getByRole("button", { name: "注册账号", exact: true })
        .click();
      await visitor
        .getByLabel("用户名", { exact: true })
        .fill(`self-${browserName}`);
      await visitor.getByLabel("密码", { exact: true }).fill(randomUUID());
      await visitor.getByLabel("注册邀请码", { exact: true }).fill(siteInvite);
      await visitor
        .getByRole("button", { name: "创建账号", exact: true })
        .click();
      await visitor.getByText("注册成功，请登录。", { exact: false }).waitFor();
      await registrationContext.close();
      await admin
        .getByLabel("注册策略", { exact: true })
        .selectOption("closed");
      await admin
        .getByRole("button", { name: "保存站点设置", exact: true })
        .click();
      // 提交中的按钮转圈而不是换文字（换文字会让宽度跳），所以同步信号是
      // data-busy 消失，而不是文案变回来。
      await admin
        .locator('button[data-busy="true"]')
        .waitFor({ state: "detached" });
      // 网络诊断（LookingGlass）：页面把请求排给节点，节点领走并回传结果。
      // 这里由测试自己扮演 Agent —— 面板侧并不知道对面是谁。
      await admin.getByRole("link", { name: "网络诊断", exact: true }).click();
      // 显式选中本轮这台机器：同一个数据库里还留着前几个浏览器引擎注册的
      // 同名节点，页面的默认选中项并不保证是本轮的这一个。
      await admin
        .getByLabel("诊断节点", { exact: true })
        .selectOption(registered.node_id);
      // 方式下拉走真实点击：用户报的缺陷就是这一处点不动。
      await pickOption(admin, "诊断方式", "tcping");
      await admin.getByLabel("诊断目标", { exact: true }).fill("127.0.0.1:9");
      await admin
        .getByRole("button", { name: "开始诊断", exact: true })
        .click();
      // 等请求真正落库再让节点来领 —— click() 在异步提交完成前就返回了。
      await admin.getByText("诊断编号", { exact: false }).waitFor();
      const pending = await (
        await admin.request.post(base + "/api/v1/agent/looking-glass", {
          headers: { Authorization: `Bearer ${registered.token}` },
        })
      ).json();
      assert.ok(pending.request?.id, "节点应当领到这条诊断");
      assert.match(
        pending.request.target,
        /^127.0.0.1:9$/,
        "下发的目标应当是规范化之后的值",
      );
      await admin.request.post(base + "/api/v1/agent/looking-glass/result", {
        headers: { Authorization: `Bearer ${registered.token}` },
        data: { ...pending.request, output: "第 1 次：成功（1 ms）\n" },
      });
      await admin.getByText("第 1 次：成功", { exact: false }).waitFor();
      // 非法目标在创建时就被拒绝，且不会排进队列。
      await admin
        .getByLabel("诊断目标", { exact: true })
        .fill("example.com; id");
      await admin
        .getByRole("button", { name: "开始诊断", exact: true })
        .click();
      await admin.getByText("主机名不合法", { exact: false }).waitFor();

      // A second registered fixture node is used only as an exit directory entry.
      const fixtureHeaders = { Origin: base, "X-Requested-With": "fetch" };
      const userRecord = (
        await (
          await admin.request.get(base + "/api/v1/users?page_size=100")
        ).json()
      ).items.find((x) => x.username === username);
      const exitGroup = await (
        await admin.request.post(base + "/api/v1/groups", {
          headers: fixtureHeaders,
          data: {
            name: `exit-group-${browserName}`,
            type: "exit",
            identity_group_ids: [userRecord.identity_group_id],
            multiplier: "1",
            port_min: 10000,
            port_max: 60000,
          },
        })
      ).json();
      const exitEnrollment = await (
        await admin.request.post(base + "/api/v1/nodes/enrollment", {
          headers: fixtureHeaders,
          data: { name: `exit-${browserName}`, group_ids: [exitGroup.id] },
        })
      ).json();
      const exitRegistration = await (
        await admin.request.post(base + "/api/v1/agent/register", {
          data: {
            token: exitEnrollment.token,
            name: `exit-${browserName}`,
            capabilities: ["tcp", "tls"],
          },
        })
      ).json();
      await admin.request.post(base + "/api/v1/agent/probe", {
        headers: { Authorization: `Bearer ${exitRegistration.token}` },
        data: { sampled_at: new Date().toISOString() },
      });
      await admin.getByRole("link", { name: "出口管理", exact: true }).click();
      await admin
        .getByRole("button", { name: "新增出口", exact: true })
        .click();
      await admin
        .getByLabel("出口名称", { exact: true })
        .fill(`managed-${browserName}`);
      await admin
        .getByLabel("出口设备组", { exact: true })
        .selectOption(exitGroup.id);
      await admin
        .getByLabel("出口服务器", { exact: true })
        .selectOption(exitRegistration.node_id);
      await admin
        .getByLabel("出口端点", { exact: true })
        .fill("exit.example.test:443");
      await admin
        .getByLabel("TLS 服务器名称", { exact: true })
        .fill("exit.example.test");
      await admin
        .getByLabel("出口凭据", { exact: true })
        .fill("fixture-exit-secret-only");
      await admin
        .getByRole("button", { name: "保存出口", exact: true })
        .click();
      await admin.locator("dialog").waitFor({ state: "detached" });
      await user.getByRole("link", { name: "转发规则", exact: true }).click();
      await user.getByRole("button", { name: "新增", exact: true }).click();
      await user.getByLabel("名称", { exact: true }).fill("managed draft");
      await user
        .getByLabel("入口服务器", { exact: true })
        .selectOption(registered.node_id);
      await user
        .getByLabel("设备组", { exact: true })
        .selectOption(savedGroup.id);
      await user
        .getByLabel("出口选择", { exact: true })
        .selectOption(exitGroup.id);
      await user.getByLabel("监听地址", { exact: true }).fill(":10022");
      await user
        .getByLabel("目标地址", { exact: true })
        .fill("target.example.test:443");
      await user.getByLabel("启用规则", { exact: true }).uncheck();
      await user
        .getByRole("group", { name: "Proxy Protocol", exact: true })
        .getByLabel("发送", { exact: true })
        .selectOption("v2");
      await save(user);
      const managedRules = await (
        await user.request.get(base + "/api/v1/rules")
      ).json();
      const managedRule = managedRules.items.find(
        (x) => x.name === "managed draft",
      );
      assert.ok(managedRule.selected_exit_id);
      assert.equal(managedRule.tunnel, undefined);
      assert.equal(managedRule.proxy_protocol.send, "v2");
      await admin.getByRole("link", { name: "设备组", exact: true }).click();
      await admin
        .getByRole("row")
        .filter({
          has: admin.getByRole("cell", {
            name: `group-${browserName}`,
            exact: true,
          }),
        })
        .getByRole("button", { name: "删除", exact: true })
        .click();
      await admin
        .getByRole("button", { name: "确认删除", exact: true })
        .click();
      await admin
        .locator("dialog")
        .getByRole("alert")
        .filter({ hasText: "转发规则" })
        .waitFor();
      await admin.keyboard.press("Escape");
      await admin.locator("dialog").waitFor({ state: "detached" });
      await user.getByRole("link", { name: "运营与任务", exact: true }).click();
      await user.getByText("导入规则", { exact: true }).click();
      const importDraft = {
        name: "preview draft",
        node_id: registered.node_id,
        group_id: savedGroup.id,
        network: "tcp",
        transport: "direct",
        listen: ":10023",
        target: "127.0.0.1:8080",
        enabled: false,
      };
      await user
        .getByLabel("规则 JSON", { exact: true })
        .fill(JSON.stringify([importDraft]));
      await user.getByRole("button", { name: "预览导入", exact: true }).click();
      await user.getByText("第 1 条 · 将新增", { exact: false }).waitFor();
      assert.equal(
        (
          await (await user.request.get(base + "/api/v1/rules")).json()
        ).items.some((x) => x.name === "preview draft"),
        false,
      );
      await user
        .getByRole("button", { name: "确认并创建导入任务", exact: true })
        .click();
      await user.getByText("导入任务已创建。", { exact: false }).waitFor();
      let imported;
      for (let attempt = 0; attempt < 50; attempt++) {
        imported = (
          await (await user.request.get(base + "/api/v1/rules")).json()
        ).items.find((x) => x.name === "preview draft");
        if (imported) break;
        await user.waitForTimeout(100);
      }
      assert.ok(imported);
      await user
        .getByLabel("导入方式", { exact: true })
        .selectOption("update_by_port");
      await user
        .getByLabel("规则 JSON", { exact: true })
        .fill(
          JSON.stringify([
            { ...importDraft, name: "updated draft", target: "127.0.0.1:8081" },
          ]),
        );
      await user.getByRole("button", { name: "预览导入", exact: true }).click();
      await user.getByText("第 1 条 · 将更新", { exact: false }).waitFor();
      await user
        .getByRole("button", { name: "确认并创建导入任务", exact: true })
        .click();
      let updated;
      for (let attempt = 0; attempt < 50; attempt++) {
        updated = (
          await (await user.request.get(base + "/api/v1/rules")).json()
        ).items.find(
          (x) => x.id === imported.id && x.target === "127.0.0.1:8081",
        );
        if (updated) break;
        await user.waitForTimeout(100);
      }
      assert.ok(updated);
      await admin
        .getByRole("link", { name: "运营与任务", exact: true })
        .click();
      await admin
        .getByLabel("购买记录 ID", { exact: true })
        .fill(beforeAddon.id);
      await admin.getByLabel("退回金额（分）", { exact: true }).fill("100");
      await admin
        .getByLabel("套餐退款原因", { exact: true })
        .fill("browser functional regression");
      await admin
        .getByRole("button", { name: "退回钱包并调整佣金", exact: true })
        .click();
      await admin
        .getByText("套餐退款已入钱包，配额和相关佣金已同步调整。", {
          exact: false,
        })
        .waitFor();
      assert.ok(
        (
          await (
            await admin.request.get(
              base + `/api/v1/purchases/${beforeAddon.id}/funding`,
            )
          ).json()
        ).items.some((x) => x.refunded_cents === "100"),
      );
      await user.getByRole("link", { name: "账号与 API", exact: true }).click();
      await user.getByRole("button", { name: "创建 Token" }).click();
      await user
        .getByLabel("Token 名称", { exact: true })
        .fill("integration-key");
      await user.getByRole("button", { name: "确认提交" }).click();
      const apiToken = await user.getByLabel("API Token 密钥").inputValue();
      assert.ok(apiToken.length > 20);
      // 探针页面预留的设备地址接口。客户脚本拿的就是这把 Token：它只能看到
      // 持有人自己那一组机器，返回的是机器当前上报的对外地址。
      const devices = await (
        await fetch(base + "/api/v1/online/device/ip/list", {
          headers: { Authorization: `Bearer ${apiToken}` },
        })
      ).json();
      const simulated = devices.items.find((d) => d.address === "203.0.113.99");
      assert.ok(simulated, "设备地址接口必须给出本组机器当前的 IP");
      assert.equal(simulated.node_id, registered.node_id);
      assert.equal(simulated.online, true);
      assert.equal(simulated.family, "ipv4");
      assert.equal(
        (
          await fetch(base + "/online/device/ip/list", {
            headers: { Authorization: `Bearer ${apiToken}` },
          })
        ).status,
        200,
        "客户脚本用的不带前缀路径也要可用",
      );
      assert.equal(
        (await fetch(base + "/api/v1/online/device/ip/list")).status,
        401,
        "没有 Token 取不到设备地址",
      );
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
      // 管理员给账号发凭据：建完账号就能把密钥交给用户，明文只在这一次出现。
      // 上一步退款留下的提示浮层会盖住侧边导航，先收起来再点。
      const adminToast = admin.getByRole("button", {
        name: "关闭通知",
        exact: true,
      });
      if (await adminToast.count()) await adminToast.click();
      await admin.getByRole("link", { name: "用户管理", exact: true }).click();
      const userRow = admin.locator("tr", { hasText: username });
      await userRow.getByRole("button", { name: "API 凭据" }).click();
      await admin
        .getByLabel("凭据名称", { exact: true })
        .fill(`admin-issued-${browserName}`);
      await admin.getByRole("checkbox", { name: "永久有效（不过期）" }).check();
      await admin
        .getByRole("button", { name: "生成凭据", exact: true })
        .click();
      const issued = await admin.getByLabel("API Token 密钥").inputValue();
      assert.ok(issued.length > 20);
      await admin.getByRole("button", { name: "已复制，继续管理" }).click();
      assert.equal(
        await admin.locator("dialog table tbody tr").count(),
        1,
        "同一个账号的凭据应当列在一起，便于重置",
      );
      assert.match(
        await admin.locator("dialog table tbody tr").first().innerText(),
        /永久有效/,
      );
      assert.equal(
        (
          await fetch(base + "/api/v1/online/device/ip/list", {
            headers: { Authorization: `Bearer ${issued}` },
          })
        ).status,
        200,
        "管理员发出的凭据同样可以取设备地址",
      );
      // 重置之后旧密钥立刻失效，新密钥也只显示这一次。
      await admin.getByRole("button", { name: "重置", exact: true }).click();
      const rotated = await admin.getByLabel("API Token 密钥").inputValue();
      assert.notEqual(rotated, issued);
      await admin.getByRole("button", { name: "已复制，继续管理" }).click();
      for (const stale of [issued]) {
        assert.equal(
          (
            await fetch(base + "/api/v1/online/device/ip/list", {
              headers: { Authorization: `Bearer ${stale}` },
            })
          ).status,
          401,
          "重置后旧密钥必须失效",
        );
      }
      assert.equal(
        (
          await fetch(base + "/api/v1/online/device/ip/list", {
            headers: { Authorization: `Bearer ${rotated}` },
          })
        ).status,
        200,
      );
      await admin.getByRole("button", { name: "撤销", exact: true }).click();
      await admin
        .getByText("这个账号还没有 API 凭据。", { exact: true })
        .waitFor();
      // 用完就关：留着的对话框会挡住后面的页面操作。
      await admin.getByRole("button", { name: "关闭对话框" }).click();
      await admin.locator("dialog").waitFor({ state: "detached" });
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
            ["operations", "运营与任务"],
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
        `${browserName}: REAL Go/SQLite/embedded UI PASS (login, user/group/enrollment, simulated Agent/probe privacy, persisted history fixture API/permissions/chart, zero wallet + insufficient funds, redeem credit, webhook CRUD/mute/enable + alert policy/events, export task result, auto-renew toggle, plan limits/edit/add-on purchase, site/invitation registration, managed exits + Proxy Protocol editor, import preview/port update, purchase refund funding, three-hop editor, Token isolation/revocation, admin-issued token issue/reset/revoke, device address API permission, password, disable + session revoke)`,
      );
      await userContext.close();
    } finally {
      await browser.close();
    }
  }
} finally {
  service.kill();
}
