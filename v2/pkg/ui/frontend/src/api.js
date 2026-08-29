// Thin fetch wrapper: every call is JSON in, JSON (or nothing) out, and
// a non-2xx response throws an Error carrying the server's own message
// so callers can hand it straight to showError.
export default async function api(path, opts) {
  const res = await fetch(path, Object.assign({ headers: { "Content-Type": "application/json" } }, opts));
  const isJSON = (res.headers.get("Content-Type") || "").includes("application/json");
  const body = isJSON ? await res.json() : null;
  if (!res.ok) {
    throw new Error((body && body.error) || `${res.status} ${res.statusText}`);
  }
  return body;
}
