import type { FetchAdapter } from "@persona/extension/ui";
import { repositoryName, repositoryPath, limits, parseRepository, parseSearch, parseTags, ReadError, searchPath, tagsPath, type DetailResult, type LimitOptions, type SearchResult } from "./model.ts";
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
export class DockerHub {
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
    if (item && this.monotonic() - item.saved < fresh) { item.used = this.monotonic(); return item; }
    return undefined;
  }
  private save(path: string, raw: unknown, at: number) {
    const bytes = new TextEncoder().encode(JSON.stringify(raw)).byteLength;
    const old = this.cache.get(path);
    if (old) { this.bytes -= old.bytes; this.cache.delete(path); }
    if (bytes > this.limits.cacheBytes) return;
    const stamp = this.monotonic();
    this.cache.set(path, { raw, at, saved: stamp, used: stamp, bytes }); this.bytes += bytes;
    while (this.cache.size > this.limits.cache || this.bytes > this.limits.cacheBytes) {
      let oldest: string | undefined;
      for (const [key, value] of this.cache) if (!oldest || value.used < this.cache.get(oldest)!.used) oldest = key;
      if (oldest === undefined) break;
      this.bytes -= this.cache.get(oldest)!.bytes; this.cache.delete(oldest);
    }
  }
  private async response(path: string, signal: AbortSignal): Promise<unknown> {
    let response: Response;
    try {
      response = await this.fetch(`https://hub.docker.com${path}`, { method: "GET", headers: { Accept: "application/json", "User-Agent": "persona-dockerhub/0.1.0 (https://github.com/nwverse/persona-extensions)" }, signal, redirect: "error" });
    } catch (error) { checkSignal(signal); throw error instanceof ReadError ? error : new ReadError("network"); }
    if (response.status === 401 || response.status === 403 || response.status === 429) {
      await response.body?.cancel().catch(() => {});
      throw new ReadError("rate_limited");
    }
    if (response.status === 404) {
      await response.body?.cancel().catch(() => {});
      throw new ReadError("not_found");
    }
    if (response.status !== 200) {
      await response.body?.cancel().catch(() => {});
      throw new ReadError("http", response.status);
    }
    if (!response.body) throw new ReadError("bad_answer");
    const reader = response.body.getReader(), chunks: Uint8Array[] = [];
    let length = 0;
    const cancel = () => { void reader.cancel().catch(() => {}); };
    if (signal.aborted) cancel(); else signal.addEventListener("abort", cancel, { once: true });
    try {
      while (true) {
        checkSignal(signal);
        const chunk = await reader.read(); checkSignal(signal);
        if (chunk.done) break;
        length += chunk.value.byteLength;
        if (length > this.limits.response) { await reader.cancel().catch(() => {}); throw new ReadError("bad_answer"); }
        chunks.push(chunk.value);
      }
    } catch (error) {
      checkSignal(signal);
      throw error instanceof ReadError ? error : new ReadError("network");
    } finally { signal.removeEventListener("abort", cancel); reader.releaseLock(); }
    const data = new Uint8Array(length); let offset = 0;
    for (const chunk of chunks) { data.set(chunk, offset); offset += chunk.byteLength; }
    let text: string;
    try { text = new TextDecoder("utf-8", { fatal: true }).decode(data); } catch { throw new ReadError("bad_answer"); }
    try { return JSON.parse(text); } catch { throw new ReadError("bad_answer"); }
  }
  private async read<T>(path: string, fresh: number, signal: AbortSignal | undefined, parse: (raw: unknown, at: number) => T): Promise<{ value: T; at: number; stale?: ReadError }> {
    checkSignal(signal);
    const cached = this.cached(path, fresh); if (cached) return { value: parse(cached.raw, cached.at), at: cached.at };
    const run = this.queue.then(async () => {
      checkSignal(signal);
      const again = this.cached(path, fresh); if (again) return { value: parse(again.raw, again.at), at: again.at };
      const pause = Math.max(0, this.limits.gap - (this.monotonic() - this.lastRequest));
      if (pause) await new Promise<void>((resolve, reject) => {
        const cancel = () => { clearTimeout(timer); reject(cancellation(signal!)); };
        const timer = setTimeout(() => { signal?.removeEventListener("abort", cancel); resolve(); }, pause);
        if (signal?.aborted) cancel(); else signal?.addEventListener("abort", cancel, { once: true });
      });
      checkSignal(signal);
      const request = budget(signal, this.limits.request);
      try {
        const raw = await this.response(path, request.signal);
        const at = this.now();
        const value = parse(raw, at);
        this.save(path, raw, at); this.lastOK = at; this.lastError = undefined;
        return { value, at };
      } catch (error) {
        if (signal?.aborted && !(signal.reason instanceof ReadError)) {
          throw error instanceof Error && error.name === "AbortError" ? error : new DOMException("Cancelled", "AbortError");
        }
        const failure = error instanceof ReadError ? error
          : signal?.aborted && signal.reason instanceof ReadError ? signal.reason
          : request.signal.aborted ? (request.signal.reason instanceof ReadError ? request.signal.reason : new ReadError("slow"))
          : error instanceof Error && error.name === "AbortError" ? error
          : new ReadError("network");
        if (failure instanceof ReadError && failure.code !== "not_found") {
          this.lastError = { code: failure.code, status: failure.status, at: this.monotonic() };
        }
        throw failure;
      } finally { this.lastRequest = this.monotonic(); request.dispose(); }
    });
    this.queue = run.then(() => {}, () => {});
    try { return await run; }
    catch (error) {
      if (error instanceof Error && error.name === "AbortError") throw error;
      const old = this.cache.get(path);
      if (old && (!(error instanceof ReadError) || error.code !== "not_found")) {
        old.used = this.monotonic();
        return { value: parse(old.raw, old.at), at: old.at, stale: error instanceof ReadError ? error : new ReadError("network") };
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
    if (!Number.isSafeInteger(limit) || limit < 1 || limit > 20) throw new ReadError("invalid");
    const path = searchPath(query, limit);
    const rd = await this.read(path, refresh ? 0 : this.limits.fresh, signal, (raw, at) => parseSearch(raw, at));
    return { ...rd.value, images: rd.value.images.slice(0, limit), stale: rd.stale };
  }
  async repository(name: string, refresh = false, signal?: AbortSignal): Promise<DetailResult> {
    name = repositoryName(name);
    const rd = await this.read(repositoryPath(name), refresh ? 0 : this.limits.fresh, signal, raw => parseRepository(raw, name));
    const tagRead = await this.read(tagsPath(name), refresh ? 0 : this.limits.fresh, signal, raw => parseTags(raw, name));
    return { image: { ...rd.value, tags: tagRead.value }, at: tagRead.at, ...(rd.stale ?? tagRead.stale ? { stale: rd.stale ?? tagRead.stale } : {}) };
  }
  cacheStats() { return { entries: this.cache.size, bytes: this.bytes }; }
}
