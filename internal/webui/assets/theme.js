/**
 * 防主题闪烁（FOUC）。
 *
 * 这段是**经典脚本**，由 Vite 注入 <head> 最前面，在样式表和模块 bundle
 * 之前执行：Vue 挂载需要几百毫秒，不在这里先涂上底色，用户会看到一瞬间的
 * 亮色再变暗 —— 这是暗色面板最常见也最廉价的一种「不高级感」。
 *
 * 默认暗色（画布 #07080b）：这套设计语言的使用场景就是近黑画布 + 环境光。
 * 面板自身的设置优先，存储不可用时退回暗色，绝不留白屏。
 */
(function () {
  var root = document.documentElement;
  var canvas = { dark: "#07080b", light: "#eceff5" };

  function paint(theme) {
    root.dataset.theme = theme;
    var meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.setAttribute("content", canvas[theme]);
  }

  try {
    var stored = localStorage.getItem("panel-theme");
    paint(stored === "light" || stored === "dark" ? stored : "dark");
    var accent = localStorage.getItem("panel-accent");
    var known = ["blue", "teal", "violet", "magenta", "amber", "graphite"];
    if (known.indexOf(accent) >= 0) root.dataset.accent = accent;
  } catch (e) {
    paint("dark");
  }
})();
