import { createRouter, createWebHashHistory } from "vue-router";
import Dashboard from "./pages/Dashboard.vue";
import Resources from "./pages/Resources.vue";
import Probes from "./pages/Probes.vue";
import Commerce from "./pages/Commerce.vue";
import Operations from "./pages/Operations.vue";
import Settings from "./pages/Settings.vue";
import Exits from "./pages/Exits.vue";
import Account from "./pages/Account.vue";
import LookingGlass from "./pages/LookingGlass.vue";
import ApiDocs from "./pages/ApiDocs.vue";
import { state } from "./core/state";
import { rotateRequests } from "./core/api";
import { clear } from "./core/motion";
export const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: "/settings", component: Settings },
    { path: "/exits", component: Exits },
    { path: "/", redirect: "/overview" },
    { path: "/overview", component: Dashboard },
    { path: "/probes", component: Probes },
    { path: "/commerce", component: Commerce },
    { path: "/operations", component: Operations },
    { path: "/wallet", redirect: "/commerce" },
    { path: "/account", component: Account },
    { path: "/diagnostics", component: LookingGlass },
    { path: "/api-docs", component: ApiDocs },
    {
      path: "/:resource(rules|nodes|groups|identity-groups|users|audit)",
      component: Resources,
    },
    { path: "/:pathMatch(.*)*", redirect: "/overview" },
  ],
});
router.beforeEach((to, from) => {
  if (state.modalOpen) return false;
  if (to.path === "/api-docs" && from.path === to.path) return true;
  rotateRequests();
  clear();
  return true;
});
router.afterEach((to, from) => {
  if (to.path === "/api-docs" && from.path === to.path) return;
  document.getElementById("view")?.scrollTo(0, 0);
});
