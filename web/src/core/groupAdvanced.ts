export const advancedSections = [
  {
    title: "入站屏蔽选项",
    fields: [
      {
        key: "allowed_host",
        value: [],
        description: "Host / SNI 白名单，与其他入站屏蔽选项冲突。",
      },
      { key: "blocked_host", value: [], description: "Host / SNI 黑名单。" },
      { key: "blocked_path", value: [], description: "HTTP Path 黑名单。" },
      {
        key: "blocked_protocol",
        value: [],
        description: "应用协议黑名单，当前支持 http、socks。",
      },
    ],
  },
  {
    title: "入站 TLS 策略",
    fields: [
      {
        key: "tls_inbound_policy",
        value: 0,
        description:
          "0：宽松模式，允许普通规则；1：只允许 TLS 入站规则；2：只允许 TLS 入站规则，且只允许管理员独立监听端口。",
      },
      {
        key: "tls_reject_empty_sni",
        value: false,
        description: "TLS 防扫策略，是否拒绝空 SNI 连接。",
      },
    ],
  },
  {
    title: "UDP 选项",
    fields: [
      { key: "disable_udp", value: false, description: "是否禁用 UDP。" },
      {
        key: "udp_over_tcp",
        value: false,
        description: "是否通过 TCP 承载 UDP；不能与 disable_udp 同时启用。",
      },
    ],
  },
  {
    title: "对端地址优先度选项",
    fields: [
      {
        key: "ipv6_group",
        value: [],
        description: "对端地址优先度使用的设备组列表。",
      },
    ],
  },
  {
    title: "故障转移",
    fields: [
      {
        key: "max_fail",
        value: 3,
        description:
          "入口连接隧道出口或入口直出时，开始转移前容忍的最大连续失败次数。",
      },
      {
        key: "fail_timout_sec",
        value: 30,
        description: "入口连接隧道出口或入口直出时的转移时长，单位为秒。",
      },
    ],
  },
  {
    title: "反向隧道选项",
    fields: [
      {
        key: "reverse_group",
        value: [],
        description: "反向隧道使用的设备组列表。",
      },
      {
        key: "protocol",
        value: "tls",
        description: "反向隧道协议，默认 tls；可选 tls、tls_simple、ws、http。",
      },
      { key: "tls", value: {}, description: "反向隧道 TLS 配置对象。" },
    ],
  },
];

type Settings = Record<string, unknown>;

export function initialGroupAdvanced(group: Record<string, unknown>): Settings {
  if (group.advanced && typeof group.advanced === "object")
    return normalize({ ...group.advanced });
  const defaults: Settings = Object.fromEntries(
    advancedSections.flatMap((section) =>
      section.fields.map((field) => [field.key, field.value]),
    ),
  );
  // Keep application blocks from groups created before the separate editor.
  defaults.blocked_protocol = ((group.blocked_protocols || []) as string[])
    .filter((value) => ["app:http", "app:socks", "socks"].includes(value))
    .map((value) => value.replace(/^app:/, ""));
  return defaults;
}

function normalize(value: unknown): Settings {
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw new Error("额外设置参数必须是 JSON 对象。");
  const settings = value as Settings;
  if ("fail_timeout_sec" in settings) {
    if (
      "fail_timout_sec" in settings &&
      settings.fail_timout_sec !== settings.fail_timeout_sec
    )
      throw new Error("请仅保留 fail_timout_sec，两个故障转移时长不能不同。");
    settings.fail_timout_sec = settings.fail_timeout_sec;
    delete settings.fail_timeout_sec;
  }
  if (Array.isArray(settings.blocked_protocol))
    settings.blocked_protocol = settings.blocked_protocol.map((value) =>
      typeof value === "string" ? value.replace(/^app:/, "") : value,
    );
  return settings;
}

export function parseGroupAdvanced(source: string): Settings {
  let out = "";
  let quote = false;
  let escaped = false;
  let lineComment = false;
  let blockComment = false;
  for (let i = 0; i < source.length; i++) {
    const c = source[i];
    const n = source[i + 1];
    if (lineComment) {
      if (c === "\n" || c === "\r") lineComment = false;
      out += lineComment ? " " : c;
    } else if (blockComment) {
      if (c === "*" && n === "/") {
        blockComment = false;
        out += "  ";
        i++;
      } else out += c === "\n" || c === "\r" ? c : " ";
    } else if (quote) {
      out += c;
      if (escaped) escaped = false;
      else if (c === "\\") escaped = true;
      else if (c === '"') quote = false;
    } else if (c === '"') {
      quote = true;
      out += c;
    } else if (c === "/" && (n === "/" || n === "*")) {
      lineComment = n === "/";
      blockComment = n === "*";
      out += "  ";
      i++;
    } else out += c;
  }
  if (blockComment) throw new Error("块注释未结束，请补上 */。");
  try {
    return normalize(JSON.parse(out));
  } catch (error) {
    if (error instanceof SyntaxError)
      throw new Error("JSON 格式有误，请检查引号、逗号和括号。");
    throw error;
  }
}

export function formatGroupAdvanced(settings: Settings): string {
  const entries: string[] = [];
  const handled = new Set<string>();
  const entry = (key: string) =>
    `  ${JSON.stringify(key)}: ${JSON.stringify(settings[key], null, 2).replace(/\n/g, "\n  ")}`;
  for (const section of advancedSections) {
    let first = true;
    for (const field of section.fields) {
      if (!Object.hasOwn(settings, field.key)) continue;
      entries.push(
        `${first ? `  // ${section.title}\n` : ""}  // ${field.description}\n${entry(field.key)}`,
      );
      handled.add(field.key);
      first = false;
    }
  }
  for (const key of Object.keys(settings)) {
    if (!handled.has(key)) entries.push(entry(key));
  }
  return entries.length ? `{\n${entries.join(",\n")}\n}` : "{}";
}
