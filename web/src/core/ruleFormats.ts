type RuleRecord = Record<string, unknown>;

interface PortForwardRule {
  dest: string[];
  listen_port: number;
  name: string;
  proxy_protocol: number;
}

export function parsePortForwardJSONL(
  source: string,
  nodeID: string,
  groupID: string,
): RuleRecord[] {
  const lines = source
    .split(/\r?\n/)
    .map((line, index) => ({ line: line.trim(), number: index + 1 }))
    .filter(({ line }) => line !== "");
  if (!lines.length || lines.length > 1000) {
    throw Error("JSON Lines 需要包含 1-1000 条规则。");
  }
  if (!nodeID || !groupID) {
    throw Error("请选择规则导入到的入口节点和设备组。");
  }

  return lines.map(({ line, number }) => {
    let value: unknown;
    try {
      value = JSON.parse(line);
    } catch {
      throw Error(`第 ${number} 行不是有效 JSON。`);
    }
    if (!value || typeof value !== "object" || Array.isArray(value)) {
      throw Error(`第 ${number} 行必须是 JSON 对象。`);
    }
    const input = value as Partial<PortForwardRule>;
    if (
      typeof input.name !== "string" ||
      !input.name.trim() ||
      !Number.isInteger(input.listen_port) ||
      Number(input.listen_port) < 1 ||
      Number(input.listen_port) > 65535 ||
      !Array.isArray(input.dest) ||
      input.dest.length < 1 ||
      input.dest.length > 16 ||
      input.dest.some((target) => typeof target !== "string" || !target.trim())
    ) {
      throw Error(`第 ${number} 行的 name、listen_port 或 dest 无效。`);
    }

    const proxyProtocol = input.proxy_protocol ?? 0;
    if (proxyProtocol !== 0 && proxyProtocol !== 3) {
      throw Error(`第 ${number} 行的 proxy_protocol 仅支持 0 或 3。`);
    }
    const dest = (input.dest as string[]).map((target) => target.trim());
    return {
      name: input.name.trim(),
      node_id: nodeID,
      group_id: groupID,
      network: "tcp",
      transport: "direct",
      listen: `:${input.listen_port}`,
      target: dest[0],
      enabled: true,
      ...(dest.length > 1
        ? {
            backends: dest.map((target) => ({
            target,
              weight: 1,
              disabled: false,
            })),
          }
        : {}),
      ...(proxyProtocol === 3
        ? {
            proxy_protocol: {
              accept: "off",
              send: "v2",
              trusted_cidrs: [],
            },
          }
        : {}),
    };
  });
}

export function exportPortForwardJSONL(result: string): string {
  let value: unknown;
  try {
    value = JSON.parse(result);
  } catch {
    throw Error("导出结果不是有效 JSON。");
  }
  if (
    !value ||
    typeof value !== "object" ||
    !Array.isArray((value as RuleRecord).items)
  ) {
    throw Error("导出结果中没有规则列表。");
  }

  const rules = (value as { items: unknown[] }).items;
  return rules
    .map((value, index) => {
      if (!value || typeof value !== "object" || Array.isArray(value)) {
        throw Error(`第 ${index + 1} 条规则无法转换为 JSON Lines。`);
      }
      const rule = value as RuleRecord;
      const listen = typeof rule.listen === "string" ? rule.listen : "";
      const portText = listen.match(/:(\d+)$/)?.[1];
      const listenPort = Number(portText);
      const backends = Array.isArray(rule.backends) ? rule.backends : [];
      const proxy =
        rule.proxy_protocol && typeof rule.proxy_protocol === "object"
          ? (rule.proxy_protocol as RuleRecord)
          : {};
      const send = String(proxy.send || "off");
      const accept = String(proxy.accept || "off");

      if (
        rule.network !== "tcp" ||
        rule.transport !== "direct" ||
        rule.enabled === false ||
        rule.tunnel ||
        rule.shared_tls ||
        rule.exit_group_id ||
        !Number.isInteger(listenPort) ||
        listenPort < 1 ||
        listenPort > 65535 ||
        typeof rule.name !== "string" ||
        !rule.name.trim() ||
        typeof rule.target !== "string" ||
        accept !== "off" ||
        (send !== "off" && send !== "v2")
      ) {
        throw Error(`第 ${index + 1} 条规则包含此格式无法导出的设置。`);
      }

      const dest = backends.length
        ? backends.map((backend) => {
            if (!backend || typeof backend !== "object") {
              throw Error(`第 ${index + 1} 条规则的后端无效。`);
            }
            const item = backend as RuleRecord;
            if (
              item.disabled === true ||
              item.weight !== 1 ||
              typeof item.target !== "string"
            ) {
              throw Error(`第 ${index + 1} 条规则的后端权重或状态无法导出。`);
            }
            return item.target;
          })
        : [rule.target];
      if (dest.length > 16) {
        throw Error(`第 ${index + 1} 条规则的目标数量超出支持范围。`);
      }

      const output: PortForwardRule = {
        dest: dest as string[],
        listen_port: listenPort,
        name: rule.name,
        proxy_protocol: send === "v2" ? 3 : 0,
      };
      return JSON.stringify(output);
    })
    .join("\n");
}
