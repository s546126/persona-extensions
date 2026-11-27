export const limits = Object.freeze({ request: 12_000, page: 15_000, tool: 15_000, response: 1 << 20, fresh: 15 * 60_000, cache: 40, cacheBytes: 2 << 20, gap: 250 });
export type LimitOptions = { [K in keyof typeof limits]?: number };
export type ReadCode = "invalid" | "bad_answer" | "not_found" | "rate_limited" | "slow" | "network" | "http";
export class ReadError extends Error {
  constructor(readonly code: ReadCode, readonly status?: number) { super(code); }
}
export const clip = (text: string, most: number) => {
  const limit = Math.floor(most);
  if (!(limit > 0)) return "";
  const chars = Array.from(text);
  if (chars.length <= limit) return text;
  if (limit === 1) return chars[0];
  return chars.slice(0, limit - 1).join("").trimEnd() + "…";
};
export const oneLine = (text: string) => text.replace(/\s+/gu, " ").trim();
export function crateName(value: unknown): string {
  if (typeof value !== "string" || !/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$/.test(value)) throw new ReadError("invalid");
  return value;
}
export function queryText(value: unknown): string {
  if (typeof value !== "string" || /[\u0000-\u001f\u007f\ud800-\udfff]/u.test(value)) throw new ReadError("invalid");
  const query = oneLine(value);
  if (!query || Array.from(query).length > 100) throw new ReadError("invalid");
  return query;
}
export function versionText(value: unknown): string {
  if (typeof value !== "string" || !/^[0-9A-Za-z][0-9A-Za-z.+-]{0,99}$/.test(value) || /[\s\/\\]/.test(value)) throw new ReadError("bad_answer");
  return value;
}
export function searchPath(query: string, limit = 12): string {
  if (!Number.isSafeInteger(limit) || limit < 1 || limit > 20) throw new ReadError("invalid");
  return `/api/v1/crates?${new URLSearchParams({ q: queryText(query), per_page: String(limit) })}`;
}
export function cratePath(name: string): string { return `/api/v1/crates/${crateName(name)}`; }
function object(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new ReadError("bad_answer");
  return value as Record<string, unknown>;
}
function array(value: unknown): unknown[] { if (!Array.isArray(value)) throw new ReadError("bad_answer"); return value; }
function string(value: unknown, fallback?: string): string {
  if (value == null && fallback !== undefined) return fallback;
  if (typeof value !== "string") throw new ReadError("bad_answer");
  return value;
}
function integer(value: unknown, maximum = 2_000_000_000_000): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0 || value > maximum) throw new ReadError("bad_answer");
  return value;
}
function boolean(value: unknown): boolean { if (typeof value !== "boolean") throw new ReadError("bad_answer"); return value; }
// Optional third-party links are omitted when they cannot be read as https. crates.io still serves plain-http
// repository, homepage and documentation links for older crates (nickel, objc, block), and one such link must not fail
// the lookup or the search.
function httpsURL(value: unknown): string | undefined {
  if (value == null || value === "") return undefined;
  let url: URL;
  try { url = new URL(string(value)); } catch { return undefined; }
  if (url.protocol !== "https:" || url.username || url.password || url.port || url.href.length > 4096) return undefined;
  return url.href;
}
function license(value: unknown, fallback = "Unspecified"): string {
  const license = clip(oneLine(string(value, fallback)), 120);
  if (!license || /[\u0000-\u001f\u007f\ud800-\udfff]/u.test(license)) throw new ReadError("bad_answer");
  return license;
}
function date(value: unknown): string | undefined {
  if (value == null) return undefined;
  const date = string(value);
  if (!Number.isFinite(Date.parse(date))) throw new ReadError("bad_answer");
  return date;
}
export interface CrateSummary {
  name: string; version: string; description: string; license: string; url: string;
  repository?: string; homepage?: string; documentation?: string; updated?: string;
  recentDownloads?: number; totalDownloads?: number;
}
export interface CrateDetail extends CrateSummary {
  install: string; yanked: boolean; versionDownloads?: number; published?: string;
}
export interface SearchResult { crates: CrateSummary[]; total: number; at: number; stale?: ReadError }
export interface DetailResult { crate: CrateDetail; at: number; stale?: ReadError }
function summary(value: unknown): CrateSummary {
  const item = object(value), name = crateName(item.name ?? item.id), version = versionText(item.default_version ?? item.newest_version ?? item.max_version);
  const url = `https://crates.io/crates/${name}`;
  const matched = item.license == null && item.default_version ? array(item.versions ?? []).find(raw => object(raw).num === version) : undefined;
  const licenseValue = item.license ?? (matched ? object(matched).license : undefined);
  const repoURL = httpsURL(item.repository);
  const homeURL = httpsURL(item.homepage);
  const docsURL = httpsURL(item.documentation);
  const updated = date(item.updated_at);
  return { name, version, description: clip(oneLine(string(item.description, "").replace(/[\ud800-\udfff]/gu, "")), 500), license: license(licenseValue, "See details"), url, ...(repoURL ? { repository: repoURL } : {}), ...(homeURL ? { homepage: homeURL } : {}), ...(docsURL ? { documentation: docsURL } : {}), ...(updated ? { updated } : {}), ...(item.recent_downloads == null ? {} : { recentDownloads: integer(item.recent_downloads) }), ...(item.downloads == null ? {} : { totalDownloads: integer(item.downloads) }) };
}
export function parseSearch(raw: unknown, at: number): SearchResult {
  const root = object(raw), meta = object(root.meta), total = integer(meta.total, 1_000_000_000), seen = new Set<string>();
  const crates = array(root.crates).slice(0, 20).map(value => {
    const crate = summary(value);
    if (seen.has(crate.name)) throw new ReadError("bad_answer"); seen.add(crate.name);
    return crate;
  });
  return { crates, total, at };
}
export function parseCrate(raw: unknown, at: number): CrateDetail {
  const root = object(raw), crate = object(root.crate), result = summary({ ...crate, versions: root.versions });
  const versions = array(root.versions), selected = versions.filter(value => object(value).num === result.version);
  if (selected.length !== 1) throw new ReadError("bad_answer");
  const version = object(selected[0]);
  if (version.crate != null && crateName(version.crate) !== result.name) throw new ReadError("bad_answer");
  const published = date(version.created_at);
  return { ...result, license: license(version.license), install: `cargo add ${result.name}@${result.version}`, yanked: boolean(version.yanked ?? false), ...(version.downloads == null ? {} : { versionDownloads: integer(version.downloads) }), ...(published ? { published } : {}) };
}
