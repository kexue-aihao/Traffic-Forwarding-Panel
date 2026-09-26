/**
 * 机器在界面上的「名字」。
 *
 * 机器自报的名字是 `ip-172-31-3-144` 这种主机名，运营方和用户都认不出来；他们
 * 认的是设备组。一台机器在一个组里本来就是同一个东西，所以默认就叫组名。
 * 同一个组里不止一台时才补上序号（`AWS日本-1`、`AWS日本-2`），否则几条选项读
 * 起来一模一样、根本分不出选的是哪台。
 *
 * 一台机器可能属于多个组：调用方给了 preferred（页面上正选着的那个组）就按它
 * 说，否则用它第一个有名字的组。取不到组名就退回机器名 —— 宁可显示一个丑的，
 * 也不要显示一个空的。
 */
export interface NamedNode {
  id: string;
  name: string;
  group_ids?: string[];
}
export interface NamedGroup {
  id: string;
  name: string;
}

export function groupLabels(
  nodes: NamedNode[],
  groups: NamedGroup[],
  preferred = "",
): Map<string, string> {
  const names = new Map(groups.map((group) => [group.id, group.name]));
  const groupOf = new Map<string, string>();
  const totals = new Map<string, number>();
  for (const node of nodes) {
    const ids = node.group_ids || [];
    const chosen = preferred && ids.includes(preferred) ? preferred : ids[0];
    const name = (chosen && names.get(chosen)) || "";
    groupOf.set(node.id, name);
    if (name) totals.set(name, (totals.get(name) || 0) + 1);
  }
  const seen = new Map<string, number>();
  const labels = new Map<string, string>();
  for (const node of nodes) {
    const group = groupOf.get(node.id) || "";
    if (!group) {
      labels.set(node.id, node.name);
      continue;
    }
    if ((totals.get(group) || 0) > 1) {
      const index = (seen.get(group) || 0) + 1;
      seen.set(group, index);
      labels.set(node.id, `${group}-${index}`);
    } else {
      labels.set(node.id, group);
    }
  }
  return labels;
}
