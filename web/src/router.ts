import { createRouter, createWebHashHistory } from "vue-router";
import Dashboard from "./pages/Dashboard.vue";
import Resources from "./pages/Resources.vue";
import Probes from "./pages/Probes.vue";
import Commerce from "./pages/Commerce.vue";
import Account from "./pages/Account.vue";
import { state } from "./core/state";
import { rotateRequests } from "./core/api";
import { clear } from "./core/motion";
export const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: "/", redirect: "/overview" },
    { path: "/overview", component: Dashboard },
    { path: "/probes", component: Probes },
    { path: "/commerce", component: Commerce },
    { path: "/wallet", redirect: "/commerce" },
    { path: "/account", component: Account },
    {
      path: "/:resource(rules|nodes|groups|users|audit)",
      component: Resources,
    },
    { path: "/:pathMatch(.*)*", redirect: "/overview" },
  ],
});
router.beforeEach(() => {
  if (state.modalOpen) return false;
  rotateRequests();
  clear();
  return true;
});
router.afterEach(() => {
  document.getElementById("view")?.scrollTo(0, 0);
});
