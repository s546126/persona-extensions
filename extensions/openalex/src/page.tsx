import { Button, Field, Link, Loading, Section, Text, View, type CallbackAction, type NativeAction, type Node } from "@persona/extension/ui";
import { boundedID, clip, oneLine, queryText, type SearchResult } from "./model.ts";
import { errorText, fmt, readTime, textsFor } from "./i18n.ts";
import { budget, checkSignal, type OpenAlex } from "./service.ts";
const TEXT_MAX = 2000;
const KEY_MAX = 128;
const ITEMS_MAX = 300;
const STATE_MAX = 16_384;
const FRAME_MAX = 512 * 1024;
const URL_MAX = 4096;
const utf8 = new TextEncoder();
const byteLength = (text: string) => utf8.encode(text).byteLength;
function safeLinkURL(value: unknown): string | undefined {
  if (typeof value !== "string" || !value) return undefined;
  let link: URL;
  try { link = new URL(value); } catch { return undefined; }
  if (link.protocol !== "https:" || link.username || link.password || link.port || link.href.length > URL_MAX) return undefined;
  return link.href;
}
function uniqueKey(used: Set<string>, prefix: string, raw: string): string {
  let key = boundedID(prefix, raw, KEY_MAX);
  let attempt = 2;
  while (used.has(key)) {
    key = boundedID(prefix, `${raw}#${attempt}`, KEY_MAX);
    attempt++;
  }
  used.add(key);
  return key;
}
export interface PageState { q: string; found: boolean }
export type Loaded = { kind: "loading" } | { kind: "empty" } | { kind: "result"; result: SearchResult } | { kind: "error"; error: unknown };
export const initialState: PageState = { q: "", found: false };
export function readState(raw: unknown): PageState {
  if (typeof raw !== "string" || byteLength(raw) > STATE_MAX) return initialState;
  try {
    const value = JSON.parse(raw) as Record<string, unknown>;
    if (!value || Array.isArray(value) || typeof value.q !== "string" || typeof value.found !== "boolean") return initialState;
    if (!value.found && value.q === "") return initialState;
    return { q: queryText(value.q), found: value.found };
  } catch { return initialState; }
}
export const encodeState = (state: PageState) => {
  const encoded = JSON.stringify(state);
  return byteLength(encoded) <= STATE_MAX ? encoded : JSON.stringify(initialState);
};
function safeState(state: PageState): PageState {
  try { return readState(JSON.stringify(state)); }
  catch { return initialState; }
}
export function transition(state: PageState, id: string, values: unknown = {}): PageState {
  let fields: Readonly<Record<string, unknown>>;
  if (typeof values === "string") {
    try { values = JSON.parse(values); } catch { return state; }
  }
  if (!values || typeof values !== "object" || Array.isArray(values)) return state;
  fields = values as Readonly<Record<string, unknown>>;
  if (id === "search") {
    if (typeof fields.query !== "string" || /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f\ud800-\udfff]/u.test(fields.query)) return state;
    try {
      const query = queryText(clip(oneLine(fields.query), 200));
      return { q: query, found: true };
    } catch { return state; }
  }
  if (id === "home") return initialState;
  return state;
}
export async function loadPage(service: OpenAlex, state: PageState, refresh: boolean, signal?: AbortSignal): Promise<Loaded> {
  const request = budget(signal, service.limits.page);
  try {
    if (!state.found) return { kind: "empty" };
    return { kind: "result", result: await service.search(state.q, 20, refresh, request.signal) };
  } catch (error) { checkSignal(signal); return { kind: "error", error }; }
  finally { request.dispose(); }
}
export interface PageProps { service: OpenAlex; state: PageState; loaded: Loaded; locale?: string; tz?: string; uiVersion?: number; action?: (id: string, next?: PageState, title?: string) => NativeAction | CallbackAction }
export function page(props: PageProps): Node {
  const { service, loaded, uiVersion = 0 } = props, state = safeState(props.state), stateJSON = encodeState(state), t = textsFor(props.locale ?? service.locale), tz = props.tz ?? "Local";
  const act = (id: string, next?: PageState, title?: string) => {
    const cleanTitle = title ? clip(title, TEXT_MAX) : undefined;
    return props.action?.(id, next, cleanTitle) ?? (next && uiVersion >= 3 ? { id, push: { ui: "research", state: encodeState(next), ...(cleanTitle ? { title: cleanTitle } : {}) } } : { id });
  };
  const text = (key: string, value: string, style: "body" | "caption" | "secondary" = "secondary") => <Text key={key} text={clip(value, TEXT_MAX)} style={style} />;
  const search = <Section key="openalex-search" title={clip(t.search, TEXT_MAX)}><Field key="openalex-query" name="query" label={clip(t.search, TEXT_MAX)} kind="text" placeholder={clip(t.searchHint, TEXT_MAX)} value={state.q} /><Button key="openalex-submit" title={clip(t.searchAction, TEXT_MAX)} action={act("search")} /></Section>;
  if (loaded.kind === "loading") return <View key="openalex" title={clip(t.search, TEXT_MAX)} state={stateJSON}>{search}<Section key="openalex-loading"><Loading key="openalex-loading-indicator" title={clip(t.loading, TEXT_MAX)} /></Section></View>;
  if (loaded.kind === "error") return <View key="openalex" title={clip(t.search, TEXT_MAX)} state={stateJSON}>{search}<Section key="openalex-error">{text("openalex-error-message", errorText(loaded.error, props.locale ?? service.locale))}<Button key="openalex-retry" title={clip(t.retry, TEXT_MAX)} action={act("refresh")} /></Section></View>;
  if (loaded.kind === "empty") return <View key="openalex" title={clip(t.search, TEXT_MAX)} state={stateJSON}>{search}<Section key="openalex-welcome">{text("openalex-welcome-text", t.welcome, "body")}</Section><Section key="openalex-attribution">{text("openalex-license", t.attribution, "caption")}</Section></View>;
  const result = loaded.result;
  const usedKeys = new Set(["openalex-search", "openalex-query", "openalex-submit", "openalex-empty-results", "openalex-empty", "openalex-footer", "openalex-refresh", "openalex-home", "openalex-attribution", "openalex-license"]);
  const reservedItems = 2 + 2 + 1;
  let remainingItems = Math.max(0, ITEMS_MAX - reservedItems);
  let remainingBytes = FRAME_MAX - byteLength(stateJSON) - 8192;
  const cards: Node[] = [];
  for (const work of result.works) {
    const workLink = safeLinkURL(work.id), oaLink = safeLinkURL(work.oaURL), doiLink = safeLinkURL(work.doi);
    const neededItems = (work.license ? 1 : 0) + (oaLink ? 1 : 0) + (workLink ? 1 : 0) + (doiLink ? 1 : 0);
    if (neededItems === 0 || neededItems > remainingItems) break;
    const sectionKey = uniqueKey(usedKeys, "work-", work.id);
    const licenseKey = work.license ? uniqueKey(usedKeys, "license-", work.id) : "";
    const fulltextKey = oaLink ? uniqueKey(usedKeys, "fulltext-", work.id) : "";
    const workLinkKey = workLink ? uniqueKey(usedKeys, "work-link-", work.id) : "";
    const doiKey = doiLink ? uniqueKey(usedKeys, "doi-", work.id) : "";
    const title = clip(work.title, TEXT_MAX);
    const cardFooter = clip([work.source, work.year ? fmt(t.year, { year: work.year }) : "", fmt(t.citations, { count: work.citations.toLocaleString("en-US") }), work.openAccess ? t.openAccess : t.closedAccess].filter(Boolean).join(" · "), TEXT_MAX);
    const licenseText = work.license ? clip(work.license, TEXT_MAX) : "";
    const estimatedBytes = byteLength(sectionKey) + byteLength(licenseKey) + byteLength(fulltextKey) + byteLength(workLinkKey) + byteLength(doiKey) + byteLength(title) + byteLength(cardFooter) + byteLength(licenseText) + (oaLink ? byteLength(oaLink) : 0) + (workLink ? byteLength(workLink) : 0) + (doiLink ? byteLength(doiLink) : 0) + 512;
    if (estimatedBytes > remainingBytes) break;
    remainingItems -= neededItems;
    remainingBytes -= estimatedBytes;
    cards.push(<Section key={sectionKey} title={title} footer={cardFooter}>
      {work.license ? text(licenseKey, licenseText, "caption") : null}
      {oaLink ? <Link key={fulltextKey} title={clip(t.openFulltext, TEXT_MAX)} url={oaLink} /> : null}
      {workLink ? <Link key={workLinkKey} title={clip(t.openWork, TEXT_MAX)} url={workLink} /> : null}
      {doiLink ? <Link key={doiKey} title={clip(t.openDOI, TEXT_MAX)} url={doiLink} /> : null}
    </Section>);
  }
  const readAt = readTime(result.at, tz, props.locale ?? service.locale);
  const footerText = clip(`${readAt ? fmt(t.readAt, { at: readAt }) : ""}${result.stale ? `${readAt ? " · " : ""}${errorText(result.stale, props.locale ?? service.locale)}` : ""}`, TEXT_MAX);
  return <View key="openalex" title={clip(fmt(t.results, { query: clip(state.q, 100) }), TEXT_MAX)} state={stateJSON}>{search}
    {cards.length ? cards : <Section key="openalex-empty-results">{text("openalex-empty", t.empty)}</Section>}
    <Section key="openalex-footer" footer={footerText}><Button key="openalex-refresh" title={clip(t.retry, TEXT_MAX)} action={act("refresh")} /><Button key="openalex-home" title={clip(t.search, TEXT_MAX)} action={act("home")} /></Section>
    <Section key="openalex-attribution">{text("openalex-license", t.attribution, "caption")}</Section>
  </View>;
}
