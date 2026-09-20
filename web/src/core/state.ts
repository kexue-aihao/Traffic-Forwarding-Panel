import { reactive } from "vue";
export interface User {
  id: string;
  username: string;
  role: string;
  disabled: boolean;
}
export const state = reactive({
  user: null as User | null,
  ready: false,
  notice: "",
  modalOpen: false,
});
export const adminSite = location.pathname.startsWith("/admin");
export function notice(value: string) {
  state.notice = value;
}
