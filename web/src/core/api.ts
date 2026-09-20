import { state, notice } from "./state";
let page = new AbortController();
export function rotateRequests() {
  page.abort();
  page = new AbortController();
}
export class ApiError extends Error {
  constructor(
    message: string,
    public status: number,
    public code: string,
  ) {
    super(message);
  }
}
export async function api<T>(
  path: string,
  method = "GET",
  body?: unknown,
): Promise<T> {
  let response: Response;
  try {
    response = await fetch("/api/v1" + path, {
      method,
      credentials: "same-origin",
      headers: {
        "Content-Type": "application/json",
        "X-Requested-With": "fetch",
      },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      ...(method === "GET" && !path.startsWith("/auth/")
        ? { signal: page.signal }
        : {}),
    });
  } catch (error) {
    if (error instanceof DOMException && error.name === "AbortError")
      throw error;
    throw new Error("网络连接失败，请检查连接后重试。");
  }
  if (response.status === 204) return null as T;
  const result = (await response
    .json()
    .catch(() => ({ error: "服务器返回了无法识别的响应" }))) as {
    error?: string;
    code?: string;
  };
  if (!response.ok) {
    if (response.status === 401) {
      state.user = null;
      state.modalOpen = false;
      if (state.ready) notice("登录已过期，请重新登录。");
    }
    throw new ApiError(
      result.error || "请求失败",
      response.status,
      result.code || "unknown",
    );
  }
  return result as T;
}
export function errorText(error: unknown) {
  return error instanceof Error ? error.message : "请求失败";
}
