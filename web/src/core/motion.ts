const active = new Set<Animation>();
const reduced = matchMedia("(prefers-reduced-motion: reduce)");
export function clear() {
  for (const a of active) {
    a.cancel();
    const target = (a.effect as KeyframeEffect | null)?.target;
    if (target instanceof HTMLElement)
      target.style.removeProperty("will-change");
  }
  active.clear();
}
export function reveal(element: HTMLElement) {
  if (reduced.matches || document.hidden) return;
  element.style.willChange = "opacity, transform";
  const a = element.animate(
    [
      { opacity: 0, transform: "translateY(6px)" },
      { opacity: 1, transform: "none" },
    ],
    { duration: 180, easing: "ease-out" },
  );
  active.add(a);
  void a.finished
    .catch(() => {})
    .finally(() => {
      active.delete(a);
      element.style.removeProperty("will-change");
    });
}
reduced.addEventListener("change", clear);
document.addEventListener("visibilitychange", () => {
  if (document.hidden) clear();
});
