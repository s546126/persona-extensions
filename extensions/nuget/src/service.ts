import type { FetchAdapter } from "@persona/extension/ui";
import { limits, nuspecPath, packageID, packageSearchPath, parseLeaf, parseNuspec, parseSearch, ReadError, registrationPath, searchPath, type DetailResult, type LimitOptions, type SearchResult } from "./model.ts";
import { errorText, textsFor } from "./i18n.ts";
export { limits, ReadError } from "./model.ts";
export interface ServiceOptions { now?: () => number; monotonic?: () => number; limits?: LimitOptions; locale?: string }
function cancellation(signal: AbortSignal): Error { return signal.reason instanceof ReadError ? signal.reason : new DOMException("Cancelled", "AbortError"); }
export function checkSignal(signal?: AbortSignal) { if (signal?.aborted) throw cancellation(signal); }
export function budget(parent: AbortSignal | undefined, milliseconds: number) {
  const controller = new AbortController();
  const cancel = () => controller.abort(parent?.reason ?? new DOMException("Cancelled", "AbortError"));
  if (parent?.aborted) cancel(); else parent?.addEventListener("abort", cancel, { once: true });
  const timer = setTimeout(() => controller.abort(new ReadError("slow")), milliseconds);
  return { signal: controller.signal, dispose: () => { clearTimeout(timer); parent?.removeEventListener("abort", cancel); } };
}
type Cached = { raw: unknown; at: number; saved: number; used: number; bytes: number };
export class Nuget {
  readonly locale: string;
  readonly now: () => number;
  readonly monotonic: () => number;
  readonly limits: { [K in keyof typeof limits]: number };
  private readonly cache = new Map<string, Cached>();
  private bytes = 0;
  private lastRequest = -Infinity;
  private queue: Promise<void> = Promise.resolve();
  private lastOK: number | undefined;
  private lastError: { code: ReadError["code"]; status?: number; at: number } | undefined;
  constructor(private readonly fetch: FetchAdapter, _config: unknown = {}, options: ServiceOptions = {}) {
    this.locale = options.locale ?? "en";
    this.now = options.now ?? Date.now; this.monotonic = options.monotonic ?? (() => performance.now()); this.limits = { ...limits, ...options.limits };
  }
  private cached(path: string, fresh: number): Cached | undefined {
    const item = this.cache.get(path);
    const nowMono = this.monotonic();
    if (item && nowMono - item.saved < fresh) { item.used = nowMono; return item; }
    return undefined;
  }
  private save(path: string, raw: unknown, at: number) {
    const bytes = new TextEncoder().encode(JSON.stringify(raw)).byteLength;
    const old = this.cache.get(path);
    if (bytes > this.limits.cacheBytes) {
      if (old) { this.bytes -= old.bytes; this.cache.delete(path); }
      return;
    }
    if (old) this.bytes -= old.bytes;
    const nowMono = this.monotonic();
    this.cache.set(path, { raw, at, saved: nowMono, used: nowMono, bytes }); this.bytes += bytes;
    while (this.cache.size > this.limits.cache || this.bytes > this.limits.cacheBytes) {
      let oldest: string | undefined;
      for (const [key, value] of this.cache) if (!oldest || value.used < this.cache.get(oldest)!.used) oldest = key;
      if (oldest === undefined) break;
      this.bytes -= this.cache.get(oldest)!.bytes; this.cache.delete(oldest);
    }
  }
  private async response(origin: string, path: string, signal: AbortSignal, xml = false): Promise<unknown> {
    let response: Response;
    try {
      response = await this.fetch(`${origin}${path}`, { method: "GET", headers: { Accept: "application/json", "User-Agent": "persona-nuget/0.1.0 (https://github.com/nwverse/persona-extensions)" }, signal, redirect: "follow" });
    } catch (error) { checkSignal(signal); throw error instanceof ReadError ? error : new ReadError("network"); }
    let finalURL: URL;
    try { finalURL = new URL(response.url || `${origin}${path}`); }
    catch { void response.body?.cancel().catch(() => {}); throw new ReadError("bad_answer"); }
    if (finalURL.protocol !== "https:" || finalURL.username || finalURL.password || finalURL.port || !["azuresearch-usnc.nuget.org", "api.nuget.org", "nuget.azure.cn"].includes(finalURL.hostname)) {
      void response.body?.cancel().catch(() => {}); throw new ReadError("bad_answer");
    }
    if (!finalURL.pathname.endsWith(".nuspec") && !finalURL.pathname.endsWith(".json") && finalURL.pathname !== "/query") {
      void response.body?.cancel().catch(() => {}); throw new ReadError("bad_answer");
    }
    if (response.status === 401 || response.status === 403 || response.status === 429) { void response.body?.cancel().catch(() => {}); throw new ReadError("rate_limited"); }
    if (response.status === 404) { void response.body?.cancel().catch(() => {}); throw new ReadError("not_found"); }
    if (response.status !== 200) { void response.body?.cancel().catch(() => {}); throw new ReadError("http", response.status); }
    if (!response.body) throw new ReadError("bad_answer");
    const reader = response.body.getReader(), chunks: Uint8Array[] = [];
    let length = 0;
    const cancel = () => { void reader.cancel().catch(() => {}); };
    signal.addEventListener("abort", cancel, { once: true });
    try {
      while (true) {
        checkSignal(signal);
        const chunk = await reader.read(); checkSignal(signal);
        if (chunk.done) break;
        length += chunk.value.byteLength;
        if (length > this.limits.response) { await reader.cancel().catch(() => {}); throw new ReadError("bad_answer"); }
        chunks.push(chunk.value);
      }
    } finally { signal.removeEventListener("abort", cancel); reader.releaseLock(); }
    const data = new Uint8Array(length); let offset = 0;
    for (const chunk of chunks) { data.set(chunk, offset); offset += chunk.byteLength; }
    let text: string;
    try { text = new TextDecoder("utf-8", { fatal: true }).decode(data); } catch { throw new ReadError("bad_answer"); }
    if (xml) return text;
    try { return JSON.parse(text); } catch { throw new ReadError("bad_answer"); }
  }
  private async read(origin: string, path: string, fresh: number, signal: AbortSignal | undefined, validate: (raw: unknown, at: number) => void, xml = false): Promise<{ raw: unknown; at: number; stale?: ReadError }> {
    checkSignal(signal);
    const key = origin + path, cached = this.cached(key, fresh); if (cached) return cached;
    const run = this.queue.then(async () => {
      checkSignal(signal);
      const again = this.cached(key, fresh); if (again) return again;
      const pause = Math.max(0, this.limits.gap - (this.monotonic() - this.lastRequest));
      if (pause) await new Promise<void>((resolve, reject) => {
        const cancel = () => { clearTimeout(timer); reject(cancellation(signal!)); };
        const timer = setTimeout(() => { signal?.removeEventListener("abort", cancel); resolve(); }, pause);
        if (signal?.aborted) cancel(); else signal?.addEventListener("abort", cancel, { once: true });
      });
      checkSignal(signal);
      const request = budget(signal, this.limits.request);
      try {
        const raw = await this.response(origin, path, request.signal, xml);
        const at = this.now(); validate(raw, at); this.save(key, raw, at); this.lastOK = at; this.lastError = undefined;
        return { raw, at };
      } catch (error) {
        if (request.signal.aborted) {
          const aborted = cancellation(request.signal);
          if (aborted instanceof ReadError) {
            this.lastError = { code: aborted.code, status: aborted.status, at: this.monotonic() };
            throw aborted;
          }
          throw error instanceof Error && error.name === "AbortError" ? error : aborted;
        }
        const failure = error instanceof ReadError ? error : error instanceof Error && error.name === "AbortError" ? error : new ReadError("network");
        if (failure instanceof ReadError && failure.code !== "not_found") this.lastError = { code: failure.code, status: failure.status, at: this.monotonic() };
        throw failure;
      } finally { this.lastRequest = this.monotonic(); request.dispose(); }
    });
    this.queue = run.then(() => {}, () => {});
    try { return await run; }
    catch (error) {
      if (error instanceof Error && error.name === "AbortError") throw error;
      const old = this.cache.get(key);
      if (old && (!(error instanceof ReadError) || error.code !== "not_found")) {
        old.used = this.monotonic();
        return { raw: old.raw, at: old.at, stale: error instanceof ReadError ? error : new ReadError("network") };
      }
      throw error;
    }
  }
  status() {
    const t = textsFor(this.locale);
    return { state: this.lastError ? this.lastError.code === "rate_limited" ? "rate_limited" : "error" : "ok", account: t.account,
      ...(this.lastOK === undefined ? {} : { last_sync: new Date(this.lastOK).toISOString() }),
      ...(this.lastError ? { error: errorText(new ReadError(this.lastError.code, this.lastError.status), this.locale) } : {}) };
  }
  async search(query: string, limit = 12, refresh = false, signal?: AbortSignal): Promise<SearchResult> {
    const rd = await this.read("https://azuresearch-usnc.nuget.org", searchPath(query, limit), refresh ? 0 : this.limits.fresh, signal, (raw, at) => { parseSearch(raw, at); });
    return { ...parseSearch(rd.raw, rd.at), stale: rd.stale };
  }
  async package(id: string, refresh = false, signal?: AbortSignal): Promise<DetailResult> {
    id = packageID(id);
    const searchRd = await this.read("https://azuresearch-usnc.nuget.org", packageSearchPath(id), refresh ? 0 : this.limits.fresh, signal, (raw, at) => { parseSearch(raw, at); });
    const found = { ...parseSearch(searchRd.raw, searchRd.at), stale: searchRd.stale };
    const exact = found.packages.find(pkg => pkg.id.toLowerCase() === id.toLowerCase());
    if (!exact) throw new ReadError("not_found");
    // IDs are case-insensitive: read on under the package's own spelling, which its nuspec, page and install command use.
    id = exact.id;
    const leaf = await this.read("https://api.nuget.org", registrationPath(id, exact.version), refresh ? 0 : this.limits.fresh, signal, raw => { parseLeaf(raw, id, exact.version); });
    const selected = parseLeaf(leaf.raw, id, exact.version);
    const metadata = await this.read("https://api.nuget.org", nuspecPath(id, exact.version), refresh ? 0 : this.limits.fresh, signal, raw => { if (typeof raw !== "string") throw new ReadError("bad_answer"); parseNuspec(raw, id, exact.version, selected.listed); }, true);
    return { package: parseNuspec(metadata.raw as string, id, exact.version, selected.listed), at: metadata.at, ...(found.stale ?? leaf.stale ?? metadata.stale ? { stale: found.stale ?? leaf.stale ?? metadata.stale } : {}) };
  }
  cacheStats() { return { entries: this.cache.size, bytes: this.bytes }; }
}
