import { ExtError, noContribution, unknownMethod, withExactParams, type ExactJSON, type Handler } from "@persona/extension";
import { clip, dateAt, type Article } from "./model.ts";
import { errorText, fmt, textsFor } from "./i18n.ts";
import { budget, ReadError, Wikipedia } from "./service.ts";
export function toolsFor(locale = "en") {
  const t = textsFor(locale);
  return [
    { name: "wikipedia_today", description: t.toolToday, input_schema: { type: "object", properties: {} } },
    { name: "wikipedia_search", description: t.toolSearch, input_schema: { type: "object", required: ["query"], properties: { query: { type: "string", description: t.query }, limit: { type: "integer", minimum: 1, maximum: 30, description: t.limit } } } },
    { name: "wikipedia_article", description: t.toolArticle, input_schema: { type: "object", required: ["title"], properties: { title: { type: "string", description: t.article } } } },
  ];
}
export const tools = toolsFor();
function line(article: Article, index: number): string {
  return `${index + 1}. ${clip(article.heading, 300)}\n   ${[clip(article.description, 300), article.views === undefined ? "" : `${article.views} views`].filter(Boolean).join(" · ")}\n   ${article.url}\n${article.extract ? "   " + clip(article.extract, 600) + "\n" : ""}`;
}
function argsFrom(raw: unknown, exact: ExactJSON | undefined, stringFields: readonly string[], allowLimit: boolean): Record<string, unknown> {
  if (exact) {
    if (exact.raw === "null") return {};
    if (!exact.raw.startsWith("{")) throw new ReadError("invalid");
    const args: Record<string, unknown> = {};
    for (const key of stringFields) for (const token of exact.fieldValues(key)) {
      if (token.raw === "null") continue;
      if (!token.raw.startsWith('"')) throw new ReadError("invalid");
      const value: unknown = JSON.parse(token.raw); if (typeof value !== "string") throw new ReadError("invalid"); args[key] = value;
    }
    if (allowLimit) {
      let hasLimit = false;
      let limit = 0n;
      for (const token of exact.fieldValues("limit")) {
        if (token.raw === "null") continue;
        try { limit = token.int64(limit); } catch { throw new ReadError("invalid"); }
        hasLimit = true;
      }
      if (hasLimit) args.limit = limit;
    }
    return args;
  }
  if (raw == null) return {};
  if (typeof raw !== "object" || Array.isArray(raw)) throw new ReadError("invalid");
  const value = raw as Record<string, unknown>;
  for (const key of stringFields) if (value[key] != null && typeof value[key] !== "string") throw new ReadError("invalid");
  if (allowLimit && value.limit != null && (typeof value.limit !== "number" || !Number.isSafeInteger(value.limit))) throw new ReadError("invalid");
  return value;
}
export async function callTool(service: Wikipedia, name: string, raw: unknown = {}, signal?: AbortSignal, exact?: ExactJSON): Promise<string> {
  if (name !== "wikipedia_today" && name !== "wikipedia_search" && name !== "wikipedia_article") throw unknownMethod(`connector.tools.${name}`);
  const stringFields = name === "wikipedia_search" ? ["query"] : name === "wikipedia_article" ? ["title"] : [];
  const args = argsFrom(raw, exact, stringFields, name === "wikipedia_search"), t = textsFor(service.locale), request = budget(signal, service.limits.tool);
  try {
    if (name === "wikipedia_today") {
      const feed = await service.today(dateAt(service.now()), false, request.signal);
      let out = `${t.featured}\n${feed.featured ? line(feed.featured, 0) : t.noFeatured + "\n"}\n${t.popular}\n`;
      out += feed.popular.slice(0, 15).map(line).join("\n");
      out += `\n${t.history}\n` + feed.events.slice(0, 8).map(event => `${event.year}. ${clip(event.text, 500)}${event.pages[0] ? `\n   ${event.pages[0].url}` : ""}`).join("\n");
      return clip(out.trim(), 29_999);
    }
    if (name === "wikipedia_search") {
      if (typeof args.query !== "string" || !args.query.trim() || Array.from(args.query).length > 200) throw new ReadError("invalid");
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
      const articles = await service.search(args.query, limit, request.signal);
      return clip(articles.length ? articles.map(line).join("\n").trim() : fmt(t.results, { query: args.query }) + "\n" + t.empty, 29_999);
    }
    if (typeof args.title !== "string") throw new ReadError("invalid");
    const article = await service.article(args.title, false, request.signal);
    return clip(`${article.heading}\n${article.description}\n${article.url}\n\n${article.extract}`.trim(), 29_999);
  } catch (error) {
    if (!(error instanceof ReadError) && !signal?.aborted && request.signal.aborted && request.signal.reason instanceof ReadError) throw request.signal.reason;
    throw error;
  } finally { request.dispose(); }
}
export function connector(getService: () => Wikipedia): Handler { return withExactParams(async (method, params: unknown, call) => {
  const service = getService();
  if (params != null && (typeof params !== "object" || Array.isArray(params))) throw new ExtError(errorText(new ReadError("invalid"), service.locale), "invalid");
  const p = (params ?? {}) as Record<string, unknown>;
  if (p.contribution != null && typeof p.contribution !== "string") throw new ExtError(errorText(new ReadError("invalid"), service.locale), "invalid");
  if (p.contribution && p.contribution !== "wikipedia") throw noContribution(String(p.contribution));
  if (method === "status") return service.status();
  if (method === "tools.list") return { tools: toolsFor(service.locale) };
  if (method === "tools.call") {
    if (typeof p.name !== "string") throw new ExtError(errorText(new ReadError("invalid"), service.locale), "invalid");
    try { return { text: await callTool(service, p.name, p.args, undefined, call?.params?.field("args")) }; }
    catch (error) { if (error instanceof ReadError) throw new ExtError(errorText(error, service.locale), error.code === "invalid" ? "invalid" : undefined); throw error; }
  }
  throw unknownMethod("connector." + method);
}); }
