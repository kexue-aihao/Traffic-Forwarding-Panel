(function () {
  try {
    var t = localStorage.getItem("panel-theme");
    var a = localStorage.getItem("panel-accent");
    document.documentElement.dataset.theme =
      t === "light" || t === "dark"
        ? t
        : matchMedia("(prefers-color-scheme: dark)").matches
          ? "dark"
          : "light";
    if (["blue", "teal", "violet", "magenta", "amber", "graphite"].includes(a))
      document.documentElement.dataset.accent = a;
  } catch (e) {
    document.documentElement.dataset.theme = "light";
  }
})();
