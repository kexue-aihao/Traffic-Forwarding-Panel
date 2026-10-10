export const advancedSections = [
  {
    title: "入站屏蔽选项",
    fields: [
      {
        key: "allowed_host",
        label: "Host / SNI 白名单",
        value: [],
        description: "Host / SNI 白名单，与其他入站屏蔽选项冲突。",
      },
      {
        key: "blocked_host",
        label: "Host / SNI 黑名单",
        value: [],
        description:
          "支持精确名称和 *.example.com；规范化大小写、端口和尾随点。",
      },
      {
        key: "blocked_path",
        label: "HTTP Path 黑名单",
        value: [],
        description:
          "逐个检查明文 HTTP/1、HTTP/2 请求；支持 *、?，HTTPS 路径不可见。",
      },
      {
        key: "blocked_protocol",
        label: "应用协议屏蔽",
        value: [],
        description:
          "拦截 HTTP/1、明文 HTTP/2、SOCKS4/5；TLS 使用可见 HTTP ALPN，UDP 仅识别数据报首部。",
      },
    ],
  },
  {
    title: "入站 TLS 策略",
    fields: [
      {
        key: "tls_inbound_policy",
        label: "TLS 入站策略",
        value: 0,
        description:
          "0：宽松模式，允许普通规则；1：只允许 TLS 入站规则；2：只允许 TLS 入站规则，且只允许管理员独立监听端口。",
      },
      {
        key: "tls_reject_empty_sni",
        label: "拒绝空 SNI",
        value: false,
        description: "TLS 防扫策略，是否拒绝空 SNI 连接。",
      },
    ],
  },
  {
    title: "UDP 选项",
    fields: [
      {
        key: "disable_udp",
        label: "禁用 UDP",
        value: false,
        description: "是否禁用 UDP。",
      },
      {
        key: "udp_over_tcp",
        label: "UDP over TCP",
        value: false,
        description:
          "带出口的 UDP 通过 TCP 承载；直连保持原生 UDP。不能与 disable_udp 同时启用。",
      },
    ],
  },
  {
    title: "对端地址优先度选项",
    fields: [
      {
        key: "ipv6_group",
        label: "IPv6 对端优先设备组",
        value: [],
        description:
          "连接所列授权设备组的双栈端点时优先 IPv6；失败回退，不改变出口权限或费用。",
      },
    ],
  },
  {
    title: "故障转移",
    fields: [
      {
        key: "max_fail",
        label: "最大连续失败次数",
        value: 3,
        description:
          "连续失败达到阈值后冷却；0 表示首次失败。单次连接可立即尝试健康备用，成功后清零。",
      },
      {
        key: "fail_timout_sec",
        label: "故障转移时长（秒）",
        value: 30,
        description:
          "失败候选的冷却秒数；0 仍保留最小 1 秒探测间隔。单目标没有可切换后端。",
      },
    ],
  },
  {
    title: "反向隧道选项",
    fields: [
      {
        key: "reverse_group",
        label: "反向隧道设备组",
        value: [],
        description:
          "在出口组指定接收主动连接的入口组；须先配置托管 reverse hub。",
      },
      {
        key: "protocol",
        label: "反向隧道协议",
        value: "tls",
        description: "反向隧道协议，默认 tls；可选 tls、tls_simple、ws、http。",
      },
      {
        key: "tls",
        label: "反向隧道 TLS 配置",
        value: {},
        description:
          "支持 enabled、server_name、min_version=1.3、alpn=[tfp-reverse-v1] 及本地 CA/证书 profile 标签。enabled=false 禁用反向服务。",
      },
    ],
  },
];

type Settings = Record<string, unknown>;

export function initialGroupAdvanced(group: Record<string, unknown>): Settings {
  if (group.advanced && typeof group.advanced === "object")
    return normalize({ ...group.advanced });
  // Keep application blocks from groups created before the separate editor.
  const migrated: Settings = {};
  const blockedProtocol = ((group.blocked_protocols || []) as string[])
    .filter((value) => ["app:http", "app:socks", "socks"].includes(value))
    .map((value) => value.replace(/^app:/, ""));
  if (blockedProtocol.length) migrated.blocked_protocol = blockedProtocol;
  return migrated;
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
