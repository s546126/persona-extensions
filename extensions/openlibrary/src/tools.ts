import { ExtError, noContribution, unknownMethod, withExactParams, type ExactJSON, type Handler } from "@persona/extension";
import { clip, queryText, ReadError, workKey, type Book, type Work } from "./model.ts";
import { errorText, fmt, textsFor } from "./i18n.ts";
import { budget, checkSignal, OpenLibrary } from "./service.ts";
export function toolsFor(locale = "en") {
  const t = textsFor(locale);
  return [
    { name: "openlibrary_search", description: t.toolSearch, input_schema: { type: "object", required: ["query"], properties: { query: { type: "string", description: t.query }, limit: { type: "integer", minimum: 1, maximum: 30, description: t.limit } } } },
    { name: "openlibrary_work", description: t.toolWork, input_schema: { type: "object", required: ["work"], properties: { work: { type: "string", description: t.work } } } },
  ];
}
export const tools = toolsFor();
function line(book: Book, index: number): string {
  return `${index + 1}. ${clip(book.title, 300)}\n   ${[book.authors.join(", "), book.year, book.editions ? `${book.editions} editions` : "", book.access].filter(Boolean).join(" · ")}\n   ${book.url}\n   Work ID: ${book.key}${book.readingURL ? `\n   Public edition: ${book.readingURL}` : ""}`;
}
function detail(work: Work): string {
  return `${work.title}\n${work.authors.join(", ")}\n${work.url}\n${work.readingURL ? `Public edition: ${work.readingURL}` : `Availability: ${work.access}`}\n\n${work.description || "No description available."}\n\nSubjects: ${work.subjects.join(", ")}`;
}
function argsFrom(raw: unknown, exact?: ExactJSON): Record<string, unknown> {
  if (exact) {
    const trimmed = exact.raw.trim();
    if (trimmed === "null") return {};
    if (!trimmed.startsWith("{")) throw new ReadError("invalid");
    const args: Record<string, unknown> = {};
    for (const key of ["query", "work"] as const) for (const token of exact.fieldValues(key)) {
      if (token.raw === "null") { delete args[key]; continue; }
      if (!token.raw.startsWith('"')) throw new ReadError("invalid");
      const value: unknown = JSON.parse(token.raw); if (typeof value !== "string") throw new ReadError("invalid"); args[key] = value;
    }
    let hasLimit = false;
    let limit = 0n;
    for (const token of exact.fieldValues("limit")) {
      if (token.raw === "null") { hasLimit = false; limit = 0n; continue; }
      try { limit = token.int64(limit); } catch { throw new ReadError("invalid"); }
      hasLimit = true;
    }
    if (hasLimit) args.limit = limit;
    return args;
  }
  if (raw == null) return {};
  if (typeof raw !== "object" || Array.isArray(raw)) throw new ReadError("invalid");
  const value = raw as Record<string, unknown>;
  if (value.query != null && typeof value.query !== "string" || value.work != null && typeof value.work !== "string") throw new ReadError("invalid");
  if (value.limit != null && (typeof value.limit !== "number" || !Number.isSafeInteger(value.limit))) throw new ReadError("invalid");
  return value;
}
export async function callTool(service: OpenLibrary, name: string, raw: unknown = {}, signal?: AbortSignal, exact?: ExactJSON): Promise<string> {
  if (name !== "openlibrary_search" && name !== "openlibrary_work") throw unknownMethod(`connector.tools.${name}`);
  const args = argsFrom(raw, exact), t = textsFor(service.locale), request = budget(signal, service.limits.tool);
  try {
    if (name === "openlibrary_search") {
      const query = queryText(args.query);
      let limit = 10;
      if (args.limit != null) {
        if (typeof args.limit === "bigint") {
          if (args.limit < 1n || args.limit > 30n) throw new ReadError("invalid");
          limit = Number(args.limit);
        } else {
          if (typeof args.limit !== "number" || !Number.isSafeInteger(args.limit) || args.limit < 1 || args.limit > 30) throw new ReadError("invalid");
          limit = args.limit;
        }
      }
      const result = await service.search(query, limit, false, request.signal);
      return clip(result.books.length ? result.books.map(line).join("\n").trim() : fmt(t.results, { query }) + "\n" + t.empty, 29_999);
    }
    const key = workKey(args.work);
    const search = await service.search(key, 30, false, request.signal);
    const base = search.books.find(book => book.key === key);
    if (!base) throw new ReadError("not_found");
    return clip(detail(await service.work(base.key, base, false, request.signal)), 29_999);
  } catch (error) {
    checkSignal(signal);
    if (!(error instanceof ReadError) && request.signal.aborted && request.signal.reason instanceof ReadError) throw request.signal.reason;
    throw error;
  } finally { request.dispose(); }
}
export function connector(getService: () => OpenLibrary): Handler { return withExactParams(async (method, params: unknown, call) => {
  const service = getService();
  if (params != null && (typeof params !== "object" || Array.isArray(params))) throw new ExtError(errorText(new ReadError("invalid"), service.locale), "invalid");
  const p = (params ?? {}) as Record<string, unknown>;
  if (p.contribution != null && typeof p.contribution !== "string") throw new ExtError(errorText(new ReadError("invalid"), service.locale), "invalid");
  if (p.contribution && p.contribution !== "openlibrary") throw noContribution(String(p.contribution));
  if (method === "status") return service.status();
  if (method === "tools.list") return { tools: toolsFor(service.locale) };
  if (method === "tools.call") {
    if (typeof p.name !== "string") throw new ExtError(errorText(new ReadError("invalid"), service.locale), "invalid");
    try { return { text: await callTool(service, p.name, p.args, undefined, call?.params?.field("args")) }; }
    catch (error) { if (error instanceof ReadError) throw new ExtError(errorText(error, service.locale), error.code === "invalid" ? "invalid" : undefined); throw error; }
  }
  throw unknownMethod("connector." + method);
}); }
