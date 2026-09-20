export const displayTimeZone = "Asia/Shanghai";
export const displayTimeZoneLabel = "上海时间 UTC+8";

const dateTime = new Intl.DateTimeFormat("zh-CN", {
  timeZone: displayTimeZone,
  year: "numeric",
  month: "2-digit",
  day: "2-digit",
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hourCycle: "h23",
});

// Numeric input is epoch milliseconds; API strings carry an explicit UTC offset.
// Use stable, readable separators across browser engines and host locales.
export function formatDateTime(value: string | number | null | undefined) {
  if (value === null || value === undefined || value === "") return "未知";
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "未知";
  const parts = Object.fromEntries(
    dateTime.formatToParts(date).map((part) => [part.type, part.value]),
  );
  return `${parts.year}-${parts.month}-${parts.day} ${parts.hour}:${parts.minute}:${parts.second}`;
}

export function formatDate(value: string | number) {
  return formatDateTime(value).split(" ")[0];
}

export function formatTime(value: string | number) {
  const formatted = formatDateTime(value);
  return formatted === "未知" ? formatted : formatted.slice(11, 16);
}
