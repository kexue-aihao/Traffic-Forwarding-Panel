import { reactive } from "vue";
export interface User {
  id: string;
  username: string;
  role: string;
  identity_group_id: string;
  disabled: boolean;
}
export const state = reactive({
  user: null as User | null,
  ready: false,
  notice: "",
  // 弹窗可能叠着（设备组的设备列表 → 节点运维），所以数个数而不是一个布尔：
  // 关掉上面那个不该把下面的滚动锁和路由离开守卫一起解掉。
  modalDepth: 0,
});
export const adminSite = location.pathname.startsWith("/admin");
export function notice(value: string) {
  state.notice = value;
}
