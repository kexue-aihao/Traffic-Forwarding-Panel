export const groupTypes = [
  {
    value: "monitor",
    label: "仅监控",
    description: "只采集设备状态和探针数据，不承担转发。",
  },
  {
    value: "entry",
    label: "入口",
    description: "接收用户连接，可直接转发或连接出口。",
  },
  {
    value: "exit",
    label: "出口",
    description: "接收入口的隧道连接，向目标服务转发。",
  },
  {
    value: "chain_exit",
    label: "链式出口",
    description: "将 2–3 个出口设备组按顺序组成一条线路，无需单独接入设备。",
  },
];

type GroupRole = { type?: unknown };
export const isEntryGroup = (g: GroupRole) => !g.type || g.type === "entry";
export const isPhysicalExitGroup = (g: GroupRole) =>
  !g.type || g.type === "exit";
export const isExitGroup = (g: GroupRole) =>
  isPhysicalExitGroup(g) || g.type === "chain_exit";
