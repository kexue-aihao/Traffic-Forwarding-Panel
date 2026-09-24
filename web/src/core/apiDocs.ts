export interface Schema {
  $ref?: string;
  type?: string | string[];
  title?: string;
  description?: string;
  format?: string;
  properties?: Record<string, Schema>;
  required?: string[];
  items?: Schema;
  anyOf?: Schema[];
  oneOf?: Schema[];
  allOf?: Schema[];
  enum?: unknown[];
  const?: unknown;
  default?: unknown;
  minimum?: number;
  maximum?: number;
  minLength?: number;
  maxLength?: number;
  minItems?: number;
  maxItems?: number;
  pattern?: string;
  readOnly?: boolean;
  writeOnly?: boolean;
  additionalProperties?: boolean | Schema;
}
export interface Parameter {
  name: string;
  in: string;
  required?: boolean;
  description?: string;
  schema?: Schema;
}
export interface MediaType {
  schema?: Schema;
  example?: unknown;
}
export interface Operation {
  operationId?: string;
  summary?: string;
  description?: string;
  "x-role"?: string;
  "x-aliases"?: string[];
  parameters?: Parameter[];
  security?: Record<string, string[]>[];
  requestBody?: { required?: boolean; content?: Record<string, MediaType> };
  responses?: Record<
    string,
    { description?: string; content?: Record<string, MediaType> }
  >;
}
export interface APIDocument {
  openapi: string;
  info: { title?: string; version?: string; description?: string };
  paths: Record<string, Record<string, Operation>>;
  components?: { schemas?: Record<string, Schema> };
}
export interface APIEntry extends Operation {
  id: string;
  method: string;
  path: string;
  category: string;
  name: string;
  role: string;
}
export const methods = [
  "GET",
  "POST",
  "PUT",
  "PATCH",
  "DELETE",
  "HEAD",
  "OPTIONS",
];
export const roles: Record<string, string> = {
  public: "公开入口",
  user: "登录账号 / 用户 Token",
  admin: "管理员",
  node: "Agent 节点",
  provider: "支付平台签名",
};
const categories: Record<string, string> = {
  site: "站点与服务",
  health: "站点与服务",
  "openapi.json": "站点与服务",
  auth: "账号与身份",
  users: "账号与身份",
  "identity-groups": "账号与身份",
  "registration-invites": "账号与身份",
  groups: "设备组",
  nodes: "服务器与节点操作",
  "node-operations": "服务器与节点操作",
  agent: "Agent 接入",
  rules: "转发与出口",
  exits: "转发与出口",
  probes: "探针与诊断",
  online: "探针与诊断",
  diagnostics: "探针与诊断",
  "looking-glass": "探针与诊断",
  plans: "套餐与权益",
  purchases: "套餐与权益",
  "addon-purchases": "套餐与权益",
  entitlement: "套餐与权益",
  "auto-renew": "套餐与权益",
  wallet: "钱包与支付",
  ledger: "钱包与支付",
  orders: "钱包与支付",
  refunds: "钱包与支付",
  "payment-channels": "钱包与支付",
  payments: "钱包与支付",
  "redeem-codes": "兑换与邀请",
  redeem: "兑换与邀请",
  referrals: "兑换与邀请",
  commissions: "兑换与邀请",
  "commission-policy": "兑换与邀请",
  webhooks: "通知与告警",
  "webhook-deliveries": "通知与告警",
  events: "通知与告警",
  "alert-policy": "通知与告警",
  tasks: "导入导出与任务",
  audit: "操作审计",
};
const resources: Record<string, string> = {
  site: "站点设置",
  health: "服务健康状态",
  "openapi.json": "OpenAPI 文档",
  users: "用户",
  "identity-groups": "身份用户组",
  "identity-group": "用户身份组",
  "registration-invites": "注册邀请码",
  groups: "设备组",
  "join-key": "设备组接入密钥",
  nodes: "服务器",
  operations: "节点操作记录",
  commands: "终端命令审计",
  rules: "转发规则",
  exits: "出口",
  probes: "探针",
  history: "探针历史",
  diagnostics: "诊断结果",
  "looking-glass": "网络诊断",
  plans: "套餐",
  purchases: "套餐购买",
  "addon-purchases": "流量叠加包购买",
  entitlement: "当前权益",
  "auto-renew": "自动续费",
  wallet: "钱包",
  ledger: "钱包账本",
  orders: "充值订单",
  refunds: "退款记录",
  "payment-channels": "支付通道",
  "redeem-codes": "兑换码",
  referrals: "邀请记录",
  commissions: "佣金记录",
  "commission-policy": "佣金政策",
  webhooks: "Webhook 订阅",
  "webhook-deliveries": "Webhook 投递记录",
  events: "事件",
  "alert-policy": "告警策略",
  tasks: "任务",
  audit: "审计记录",
  tokens: "API Token",
  config: "Agent 配置",
  control: "Agent 操作任务",
  funding: "购买资金来源",
};
const actions: Record<string, string> = {
  "/auth/login": "账号登录",
  "/auth/logout": "退出登录",
  "/auth/session": "查询当前账号",
  "/auth/password": "修改账号密码",
  "/auth/register": "注册账号",
  "/auth/captcha": "获取验证码",
  "/agent/register": "注册 Agent 节点",
  "/online/device/ip": "查询单台设备地址",
  "/online/device/ip/list": "查询设备地址列表",
  "/probes/events": "订阅探针 SSE 事件",
  "/agent/diagnostics": "领取转发诊断任务",
  "/agent/looking-glass": "领取网络诊断任务",
  "/agent/control": "领取节点操作任务",
  "/nodes/{id}/looking-glass": "发起网络诊断",
  "/looking-glass/{id}": "查询网络诊断结果",
};
const actionNames: Record<string, string> = {
  "reset-password": "重置用户密码",
  status: "修改用户状态",
  "rotate-token": "轮换节点凭据",
  enrollment: "生成节点接入凭据",
  "operation-access": "验证节点操作权限",
  terminal: "建立远程终端",
  upgrade: "升级 Agent",
  cancel: "取消任务",
  "network-diagnostic": "发起转发网络诊断",
  diagnose: "查询转发状态诊断",
  export: "导出规则",
  import: "导入规则",
  preview: "预览规则导入",
  reconcile: "核实支付状态",
  close: "关闭订单",
  refund: "申请退款",
  resolve: "处理退款或佣金",
  redeem: "兑换权益",
  bind: "绑定邀请关系",
  notify: "接收支付回调",
  ack: "确认 Agent 配置",
  usage: "上报计量",
  probe: "上报探针",
  retire: "退还计量租约",
  reset: "重置 API Token",
  result: "上报执行结果",
};
export function entries(document: APIDocument): APIEntry[] {
  return Object.entries(document.paths)
    .flatMap(([path, operations]) =>
      Object.entries(operations).flatMap(([method, operation]) => {
        method = method.toUpperCase();
        if (!methods.includes(method)) return [];
        const parts = path
          .split("/")
          .filter((part) => part && !part.startsWith("{"));
        const last = parts.at(-1) || "";
        const verb =
          (
            {
              GET: "查询",
              POST: "创建",
              PUT: "更新",
              PATCH: "更新",
              DELETE: "删除",
            } as Record<string, string>
          )[method] || method;
        return [
          {
            ...operation,
            id: operation.operationId || `${method} ${path}`,
            method,
            path: "/api/v1" + path,
            category: categories[parts[0]] || "其他接口",
            name:
              (method === "POST" && path === "/groups/{id}/join-key"
                ? "轮换设备组接入密钥"
                : "") ||
              (method === "DELETE" && parts[0] === "redeem-codes"
                ? "撤销兑换码"
                : "") ||
              actions[path] ||
              actionNames[last] ||
              `${verb}${resources[last] || resources[parts[0]] || "接口"}`,
            role:
              operation["x-role"] === "agent"
                ? "node"
                : operation["x-role"] || "public",
          },
        ];
      }),
    )
    .sort(
      (a, b) =>
        a.path.localeCompare(b.path) ||
        methods.indexOf(a.method) - methods.indexOf(b.method),
    );
}
export function resolveSchema(
  schema: Schema = {},
  definitions: Record<string, Schema> = {},
  seen: string[] = [],
): Schema {
  if (!schema.$ref) return schema;
  const name = schema.$ref.replace("#/components/schemas/", "");
  if (seen.includes(name) || !definitions[name])
    return { type: name, description: schema.description };
  const { $ref: _, ...extra } = schema;
  return {
    ...resolveSchema(definitions[name], definitions, [...seen, name]),
    ...extra,
  };
}
export function schemaType(
  input: Schema = {},
  definitions: Record<string, Schema> = {},
  depth = 0,
): string {
  if (depth > 6) return "object";
  const schema = resolveSchema(input, definitions);
  const choices = schema.anyOf || schema.oneOf;
  if (choices)
    return choices
      .map((item) => schemaType(item, definitions, depth + 1))
      .join(" | ");
  if (schema.allOf)
    return schema.allOf
      .map((item) => schemaType(item, definitions, depth + 1))
      .join(" & ");
  if (schema.type === "array")
    return `${schemaType(schema.items, definitions, depth + 1)}[]`;
  return Array.isArray(schema.type)
    ? schema.type.join(" | ")
    : schema.type || (schema.properties ? "object" : "任意类型");
}
export function schemaConstraints(schema: Schema): string {
  const parts: string[] = [];
  if (schema.format) parts.push(`格式：${schema.format}`);
  if (schema.enum)
    parts.push(
      `可选值：${schema.enum.map((item) => JSON.stringify(item)).join("、")}`,
    );
  if (schema.const !== undefined)
    parts.push(`固定值：${JSON.stringify(schema.const)}`);
  if (schema.default !== undefined)
    parts.push(`默认值：${JSON.stringify(schema.default)}`);
  for (const [key, label] of [
    ["minimum", "最小值"],
    ["maximum", "最大值"],
    ["minLength", "最短长度"],
    ["maxLength", "最长长度"],
    ["minItems", "最少项数"],
    ["maxItems", "最多项数"],
  ] as const) {
    if (schema[key] !== undefined) parts.push(`${label}：${schema[key]}`);
  }
  if (schema.pattern) parts.push(`匹配规则：${schema.pattern}`);
  if (schema.readOnly) parts.push("仅响应");
  if (schema.writeOnly) parts.push("仅请求");
  return parts.join("；");
}
export function authentication(operation: Operation): string {
  if (!operation.security?.length)
    return operation["x-role"] === "provider"
      ? "验证支付平台签名"
      : "无需登录会话；按接口要求提交验证码、接入凭据等参数";
  const names: Record<string, string> = {
    cookieSession: "Cookie 会话（tfp_session）",
    ownerBearer: "用户 Token（Authorization: Bearer <token>）",
    nodeBearer: "Agent Token（Authorization: Bearer <token>）",
  };
  return operation.security
    .map((choice) =>
      Object.keys(choice)
        .map((key) => names[key] || key)
        .join(" + "),
    )
    .join(" 或 ");
}
