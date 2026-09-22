/**
 * 位置图标：把国家/地区码画成一面旗，画不出来就退回带码的徽章。
 *
 * 为什么不用 emoji 旗子：Windows 上的 Chrome、Edge、Firefox 都没有旗形字形，
 * 「🇭🇰」会渲染成两个字母。既然目标平台里就有不支持的系统，那就自己画。
 *
 * 为什么用「条纹组合」而不是逐国手绘路径：几十个国家的旗子大多是横条、竖条、
 * 北欧十字和州徽块这几种结构，用同一套基元拼出来既短又可核对。画不出来的部分
 * （徽章、文字、复杂纹样）宁可省略，也不画错 —— 位置图标是用来区分设备的，
 * 一面明显错误的旗子比一个中性的徽章更糟。
 */
export type Shape =
  | { t: "rect"; x: number; y: number; w: number; h: number; fill: string }
  | { t: "circle"; cx: number; cy: number; r: number; fill: string }
  | { t: "path"; d: string; fill: string };

export type Flag = { shapes: Shape[]; label: string; code: string };

// 画布固定 3:2，所有坐标都按它来。
const W = 30;
const H = 20;

function rect(x: number, y: number, w: number, h: number, fill: string): Shape {
  return { t: "rect", x, y, w, h, fill };
}
function circle(cx: number, cy: number, r: number, fill: string): Shape {
  return { t: "circle", cx, cy, r, fill };
}

/** 横向等分条纹，自上而下。 */
function bandsH(colors: string[]): Shape[] {
  const h = H / colors.length;
  return colors.map((fill, i) => rect(0, i * h, W, h, fill));
}
/** 纵向等分条纹，自左向右。 */
function bandsV(colors: string[]): Shape[] {
  const w = W / colors.length;
  return colors.map((fill, i) => rect(i * w, 0, w, H, fill));
}
/** 北欧十字：底色 + 十字，可再叠一层内十字。 */
function nordic(field: string, cross: string, inner?: string): Shape[] {
  const shapes = [rect(0, 0, W, H, field), rect(9, 0, 6, H, cross), rect(0, 7, W, 6, cross)];
  if (inner) {
    shapes.push(rect(10.5, 0, 3, H, inner), rect(0, 8.5, W, 3, inner));
  }
  return shapes;
}
/** 正十字：瑞士、格鲁吉亚一类。 */
function cross(field: string, bar: string): Shape[] {
  return [rect(0, 0, W, H, field), rect(12, 0, 6, H, bar), rect(0, 7, W, 6, bar)];
}
/** 圆形居中，用于日本、孟加拉一类。 */
function disc(field: string, color: string, r = 5): Shape[] {
  return [rect(0, 0, W, H, field), circle(W / 2, H / 2, r, color)];
}
/** 五角星（近似）：位置图标只有十几像素，够看出是颗星就行。 */
function star(cx: number, cy: number, r: number, fill: string): Shape {
  const points: string[] = [];
  for (let i = 0; i < 10; i++) {
    const radius = i % 2 === 0 ? r : r * 0.42;
    const angle = -Math.PI / 2 + (i * Math.PI) / 5;
    points.push(`${(cx + radius * Math.cos(angle)).toFixed(2)},${(cy + radius * Math.sin(angle)).toFixed(2)}`);
  }
  return { t: "path", d: `M${points.join("L")}Z`, fill };
}

const RED = "#d0202f";
const DARK_RED = "#a01a26";
const BLUE = "#1f4f9c";
const LIGHT_BLUE = "#5b9bd5";
const WHITE = "#ffffff";
const BLACK = "#1b1b1f";
const GREEN = "#1f8a4c";
const YELLOW = "#f2c500";
const GOLD = "#e8b500";
const ORANGE = "#e8722c";

/** 国家/地区码 → 旗面构成。只收录常见接入地区，其余退回徽章。 */
const TABLE: Record<string, () => Shape[]> = {
  CN: () => [rect(0, 0, W, H, RED), star(6, 6, 4, YELLOW), star(12, 4, 1.6, YELLOW), star(13.5, 8, 1.6, YELLOW)],
  HK: () => [rect(0, 0, W, H, RED), circle(W / 2, H / 2, 4, WHITE), circle(W / 2, H / 2, 1.6, RED)],
  MO: () => [rect(0, 0, W, H, "#0f7a4a"), circle(W / 2, 7, 2.6, WHITE), star(W / 2, 15, 3, YELLOW)],
  TW: () => [rect(0, 0, W, H, RED), rect(0, 0, W / 2, H / 2, BLUE), circle(W / 4, H / 4, 3, WHITE)],
  JP: () => disc(WHITE, RED, 5),
  KR: () => [rect(0, 0, W, H, WHITE), circle(W / 2, H / 2, 4, RED), { t: "path", d: `M10,10a5,5 0 0,0 10,0Z`, fill: BLUE }],
  SG: () => [rect(0, 0, W, H, WHITE), rect(0, 0, W, H / 2, RED), circle(7, 5, 3, WHITE), circle(9, 5, 2.6, RED), star(13, 3.4, 1.5, WHITE), star(15.5, 5.6, 1.5, WHITE)],
  US: () => [rect(0, 0, W, H, WHITE), ...[0, 2, 4, 6].map((i) => rect(0, (i * H) / 8, W, H / 8, RED)), rect(0, 0, W * 0.4, H / 2, BLUE), ...[0, 1, 2].map((i) => star(4 + i * 4, 3.4 + i * 1.2, 1.2, WHITE))],
  CA: () => [rect(0, 0, W, H, WHITE), rect(0, 0, W / 4, H, RED), rect((W * 3) / 4, 0, W / 4, H, RED), { t: "path", d: "M15,4l1.6,3.2l2.4-0.6l-1,3l2,1.4l-3,0.6l0.4,2.6l-2.4-1.2l-2.4,1.2l0.4-2.6l-3-0.6l2-1.4l-1-3l2.4,0.6Z", fill: RED }],
  GB: () => [rect(0, 0, W, H, "#1b3a75"), rect(10, 0, 10, H, WHITE), rect(0, 6, W, 8, WHITE), rect(12, 0, 6, H, RED), rect(0, 8, W, 4, RED)],
  DE: () => bandsH([BLACK, RED, GOLD]),
  FR: () => bandsV([BLUE, WHITE, RED]),
  NL: () => bandsH([RED, WHITE, BLUE]),
  BE: () => bandsV([BLACK, YELLOW, RED]),
  LU: () => bandsH([RED, WHITE, LIGHT_BLUE]),
  CH: () => cross(RED, WHITE),
  AT: () => bandsH([RED, WHITE, RED]),
  SE: () => nordic(BLUE, YELLOW),
  NO: () => nordic(RED, WHITE, "#1b3a75"),
  DK: () => nordic(RED, WHITE),
  FI: () => nordic(WHITE, "#1b3a75"),
  IS: () => nordic("#1b3a75", WHITE, RED),
  PL: () => bandsH([WHITE, RED]),
  CZ: () => [rect(0, 0, W, H / 2, WHITE), rect(0, H / 2, W, H / 2, RED), { t: "path", d: "M0,0L13,10L0,20Z", fill: "#1b3a75" }],
  SK: () => bandsH([WHITE, "#1b3a75", RED]),
  HU: () => bandsH([RED, WHITE, GREEN]),
  RO: () => bandsV(["#1b3a75", YELLOW, RED]),
  BG: () => bandsH([WHITE, GREEN, RED]),
  GR: () => [rect(0, 0, W, H, WHITE), ...[0, 2, 4, 6, 8].map((i) => rect(0, (i * H) / 9, W, H / 9, "#2b6bb5")), rect(0, 0, W / 3, (H * 5) / 9, "#2b6bb5"), rect(W / 9, 0, W / 9, (H * 5) / 9, WHITE), rect(0, (H * 2) / 9, W / 3, H / 9, WHITE)],
  IT: () => bandsV([GREEN, WHITE, RED]),
  ES: () => bandsH([RED, GOLD, RED]),
  PT: () => [rect(0, 0, W * 0.4, H, GREEN), rect(W * 0.4, 0, W * 0.6, H, RED), circle(W * 0.4, H / 2, 3.2, GOLD)],
  IE: () => bandsV([GREEN, WHITE, ORANGE]),
  RU: () => bandsH([WHITE, "#1b3a75", RED]),
  UA: () => bandsH(["#2b6bb5", YELLOW]),
  BY: () => bandsH([RED, GREEN]),
  LT: () => bandsH([YELLOW, GREEN, RED]),
  LV: () => bandsH([DARK_RED, WHITE, DARK_RED]),
  EE: () => bandsH(["#2b6bb5", BLACK, WHITE]),
  TR: () => [rect(0, 0, W, H, RED), circle(12, 10, 4.2, WHITE), circle(14.4, 10, 3.4, RED), star(20, 10, 2.2, WHITE)],
  IL: () => [rect(0, 0, W, H, WHITE), rect(0, 3, W, 3, "#1b3a75"), rect(0, 14, W, 3, "#1b3a75")],
  AE: () => [rect(0, 0, W, H / 3, GREEN), rect(0, H / 3, W, H / 3, WHITE), rect(0, (H * 2) / 3, W, H / 3, BLACK), rect(0, 0, W / 4, H, RED)],
  SA: () => [rect(0, 0, W, H, "#146b3a"), rect(6, 12, W - 12, 2, WHITE)],
  QA: () => [rect(0, 0, W, H, WHITE), { t: "path", d: "M8,0L13,2L8,4L13,6L8,8L13,10L8,12L13,14L8,16L13,18L8,20H30V0Z", fill: "#8a1538" }],
  KW: () => [rect(0, 0, W, H / 3, GREEN), rect(0, H / 3, W, H / 3, WHITE), rect(0, (H * 2) / 3, W, H / 3, RED), { t: "path", d: `M0,0L10,6.67L10,13.33L0,20Z`, fill: BLACK }],
  IN: () => [rect(0, 0, W, H / 3, ORANGE), rect(0, (H * 2) / 3, W, H / 3, GREEN), circle(W / 2, H / 2, 2.4, "#1b3a75")],
  PK: () => [rect(0, 0, W, H, "#146b3a"), rect(0, 0, W / 4, H, WHITE), star(19, 8, 3, WHITE)],
  BD: () => [rect(0, 0, W, H, "#146b3a"), circle(13, H / 2, 5, RED)],
  TH: () => bandsH([RED, WHITE, "#1b3a75", WHITE, RED]),
  VN: () => [rect(0, 0, W, H, RED), star(W / 2, H / 2, 5, YELLOW)],
  ID: () => bandsH([RED, WHITE]),
  PH: () => [rect(0, 0, W, H / 2, "#1b3a75"), rect(0, H / 2, W, H / 2, RED), rect(0, 0, W / 3, H, WHITE), star(5, 10, 2.4, GOLD)],
};

const TABLE_MORE: Record<string, () => Shape[]> = {
  MY: () => [rect(0, 0, W, H, WHITE), ...[0, 2, 4, 6, 8, 10].map((i) => rect(0, (i * H) / 14, W, H / 14, RED)), rect(0, 0, W / 2, H / 2, "#1b3a75"), circle(9, 4.6, 2.6, YELLOW), circle(10.6, 4.6, 2.2, "#1b3a75"), star(16, 5, 2, YELLOW)],
  AU: () => [rect(0, 0, W, H, "#1b3a75"), rect(0, 0, W / 2, H / 2, "#1b3a75"), rect(6, 0, 3, H / 2, WHITE), rect(0, 3.5, W / 2, 3, WHITE), rect(7, 0, 1.6, H / 2, RED), rect(0, 4.2, W / 2, 1.6, RED), star(22, 5, 2, WHITE), star(24, 12, 2.4, WHITE), star(19, 15, 1.6, WHITE)],
  NZ: () => [rect(0, 0, W, H, "#1b3a75"), rect(0, 0, W / 2, H / 2, "#1b3a75"), rect(6, 0, 3, H / 2, WHITE), rect(0, 3.5, W / 2, 3, WHITE), rect(7, 0, 1.6, H / 2, RED), rect(0, 4.2, W / 2, 1.6, RED), star(22, 5, 2, RED), star(24, 12, 2.4, RED), star(19, 15, 1.6, RED)],
  BR: () => [rect(0, 0, W, H, GREEN), { t: "path", d: "M15,3L27,10L15,17L3,10Z", fill: YELLOW }, circle(15, 10, 4, "#1b3a75")],
  AR: () => [rect(0, 0, W, H, WHITE), rect(0, 0, W, H / 3, LIGHT_BLUE), rect(0, (H * 2) / 3, W, H / 3, LIGHT_BLUE), circle(15, 10, 2.2, GOLD)],
  CL: () => [rect(0, 0, W, H / 2, WHITE), rect(0, H / 2, W, H / 2, RED), rect(0, 0, W / 3, H / 2, "#1b3a75"), star(5, 5, 2.6, WHITE)],
  CO: () => bandsH([YELLOW, "#1b3a75", RED]),
  MX: () => bandsV([GREEN, WHITE, RED]),
  ZA: () => [rect(0, 0, W, H, "#146b3a"), rect(0, 0, W, H / 2, WHITE), rect(0, 0, W, H / 4, RED), rect(0, (H * 3) / 4, W, H / 4, "#1b3a75"), { t: "path", d: "M0,0L13,10L0,20Z", fill: "#146b3a" }],
  NG: () => bandsV([GREEN, WHITE, GREEN]),
  EG: () => bandsH([RED, WHITE, BLACK]),
  MA: () => [rect(0, 0, W, H, RED), star(W / 2, H / 2, 3.4, GREEN)],
  KE: () => bandsH([BLACK, WHITE, RED, WHITE, GREEN]),
  KZ: () => [rect(0, 0, W, H, "#00a3c4"), circle(15, 10, 3.6, YELLOW)],
  IR: () => bandsH([GREEN, WHITE, RED]),
  IQ: () => bandsH([RED, WHITE, BLACK]),
  AM: () => bandsH([RED, "#1b3a75", ORANGE]),
  GE: () => cross(WHITE, RED),
  AZ: () => bandsH(["#00a3c4", RED, GREEN]),
  CY: () => [rect(0, 0, W, H, WHITE), { t: "path", d: "M14,6l6,4l-6,4l-2,0l4,-4l-4,-4Z", fill: ORANGE }],
  MT: () => bandsV([WHITE, RED]),
  HR: () => bandsH([RED, WHITE, "#1b3a75"]),
  RS: () => bandsH([RED, "#1b3a75", WHITE]),
  SI: () => bandsH([WHITE, "#1b3a75", RED]),
  AL: () => [rect(0, 0, W, H, RED), { t: "path", d: "M15,5l1,3l3,-1l-2,3l2,3l-3,-1l-1,3l-1,-3l-3,1l2,-3l-2,-3l3,1Z", fill: BLACK }],
  PE: () => bandsV([RED, WHITE, RED]),
  VE: () => bandsH([YELLOW, "#1b3a75", RED]),
  PA: () => [rect(0, 0, W / 2, H / 2, WHITE), rect(W / 2, 0, W / 2, H / 2, RED), rect(0, H / 2, W / 2, H / 2, "#1b3a75"), rect(W / 2, H / 2, W / 2, H / 2, WHITE), star(7, 5, 2.4, "#1b3a75"), star(22, 15, 2.4, RED)],
  CR: () => bandsH(["#1b3a75", WHITE, RED, WHITE, "#1b3a75"]),
  DO: () => [rect(0, 0, W / 2, H / 2, "#1b3a75"), rect(W / 2, 0, W / 2, H / 2, RED), rect(0, H / 2, W / 2, H / 2, RED), rect(W / 2, H / 2, W / 2, H / 2, "#1b3a75"), rect(12, 0, 6, H, WHITE), rect(0, 7, W, 6, WHITE)],
  UY: () => [rect(0, 0, W, H, WHITE), ...[0, 2, 4, 6, 8].map((i) => rect(0, (i * H) / 10, W, H / 10, "#1b3a75")), rect(0, 0, W / 3, (H * 6) / 10, WHITE), circle(5, 6, 2.6, GOLD)],
};

// 两张表合成一个查找表：第一张是常见接入地区，第二张是其余的。
const FLAGS: Record<string, () => Shape[]> = { ...TABLE, ...TABLE_MORE };

/** 中文里常见的别名写法：查询接口给什么都能对上号。 */
const ALIAS: Record<string, string> = { UK: "GB", EL: "GR", EN: "GB" };

const NAMES: Record<string, string> = {
  CN: "中国大陆", HK: "中国香港", MO: "中国澳门", TW: "中国台湾",
  JP: "日本", KR: "韩国", SG: "新加坡", US: "美国", CA: "加拿大", GB: "英国",
  DE: "德国", FR: "法国", NL: "荷兰", BE: "比利时", LU: "卢森堡", CH: "瑞士",
  AT: "奥地利", SE: "瑞典", NO: "挪威", DK: "丹麦", FI: "芬兰", IS: "冰岛",
  PL: "波兰", CZ: "捷克", SK: "斯洛伐克", HU: "匈牙利", RO: "罗马尼亚",
  BG: "保加利亚", GR: "希腊", IT: "意大利", ES: "西班牙", PT: "葡萄牙",
  IE: "爱尔兰", RU: "俄罗斯", UA: "乌克兰", BY: "白俄罗斯", LT: "立陶宛",
  LV: "拉脱维亚", EE: "爱沙尼亚", TR: "土耳其", IL: "以色列", AE: "阿联酋",
  SA: "沙特", QA: "卡塔尔", KW: "科威特", IN: "印度", PK: "巴基斯坦",
  BD: "孟加拉", TH: "泰国", VN: "越南", MY: "马来西亚", ID: "印尼",
  PH: "菲律宾", AU: "澳大利亚", NZ: "新西兰", BR: "巴西", AR: "阿根廷",
  CL: "智利", CO: "哥伦比亚", MX: "墨西哥", ZA: "南非", NG: "尼日利亚",
  EG: "埃及", MA: "摩洛哥", KE: "肯尼亚", KZ: "哈萨克斯坦", IR: "伊朗",
  IQ: "伊拉克", AM: "亚美尼亚", GE: "格鲁吉亚", AZ: "阿塞拜疆", CY: "塞浦路斯",
  MT: "马耳他", HR: "克罗地亚", RS: "塞尔维亚", SI: "斯洛文尼亚", AL: "阿尔巴尼亚",
  PE: "秘鲁", VE: "委内瑞拉", PA: "巴拿马", CR: "哥斯达黎加", DO: "多米尼加",
  UY: "乌拉圭",
};

function normalize(raw: string | null | undefined): string {
  const code = (raw || "").trim().toUpperCase();
  return ALIAS[code] || code;
}

/**
 * flagFor 返回一个地区码对应的图形。认不出来的码画成中性的徽章 —— 徽章上仍然
 * 写着码，看图的人还是能区分设备，只是不冒充一面旗。空码返回 null，
 * 由调用方决定显示什么。
 */
export function flagFor(raw: string | null | undefined): Flag | null {
  const code = normalize(raw);
  if (!code) return null;
  const draw = FLAGS[code];
  const label = NAMES[code] || code;
  if (!draw) {
    return { shapes: [rect(0, 0, W, H, "#dfe4ee"), rect(0, 0, W, 4, "#98a2b7")], label, code };
  }
  return { shapes: draw(), label, code };
}

/** known 表示这个码有对应的旗面，界面上据此决定要不要额外写出地区码。 */
export function known(raw: string | null | undefined): boolean {
  return Boolean(FLAGS[normalize(raw)]);
}

export function countryName(raw: string | null | undefined): string {
  const code = normalize(raw);
  if (!code) return "";
  return NAMES[code] || code;
}
export { W as FLAG_WIDTH, H as FLAG_HEIGHT };
