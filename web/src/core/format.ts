export const displayTimeZone = "Asia/Shanghai";
export const displayTimeZoneLabel = "上海时间 UTC+8";

const byteUnits = ["B", "KB", "MB", "GB", "TB", "PB", "EB"] as const;

export function formatBytes(
  value: string | number | null | undefined,
  suffix: "" | "/s" = "",
) {
  let unit = 0;
  let amount: number;
  if (typeof value === "string") {
    if (!/^\d+$/.test(value)) return "未知";
    const bytes = BigInt(value);
    let divisor = 1n;
    while (bytes >= divisor * 1024n && unit < byteUnits.length - 1) {
      divisor *= 1024n;
      unit++;
    }
    // Truncate for display so a value below 1 TB never rounds up to 1024 GB.
    amount = Number((bytes * 100n) / divisor) / 100;
  } else {
    if (typeof value !== "number" || !Number.isFinite(value) || value < 0)
      return "未知";
    amount = value;
    while (amount >= 1024 && unit < byteUnits.length - 1) {
      amount /= 1024;
      unit++;
    }
    amount = Math.floor(amount * 100) / 100;
  }
  return `${amount} ${byteUnits[unit]}${suffix}`;
}

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
