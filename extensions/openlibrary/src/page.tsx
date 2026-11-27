import { Button, Field, Link, Loading, Row, Section, Text, View, type CallbackAction, type NativeAction, type Node } from "@persona/extension/ui";
import { boundedID, clip, plain, queryText, ReadError, workKey, type Book, type SearchResult, type Work } from "./model.ts";
import { errorText, fmt, readTime, textsFor } from "./i18n.ts";
import { budget, checkSignal, type OpenLibrary } from "./service.ts";
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
export type ViewKind = "discover" | "search" | "work";
export interface PageState { v: ViewKind; q: string; k: string; b?: Book }
export type Loaded = { kind: "loading" } | { kind: "empty" } | { kind: "search"; result: SearchResult } | { kind: "work"; work: Work } | { kind: "error"; error: unknown };
export const initialState: PageState = { v: "discover", q: "", k: "" };
function book(raw: unknown): Book | undefined {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return undefined;
  const b = raw as Record<string, unknown>;
  if (typeof b.key !== "string" || typeof b.title !== "string" || /[\ud800-\udfff]/u.test(b.title) || !Array.isArray(b.authors) || b.authors.some(x => typeof x !== "string" || /[\ud800-\udfff]/u.test(x)) ||
    typeof b.url !== "string" || b.url !== `https://openlibrary.org/works/${b.key}` || typeof b.access !== "string" ||
    !["public", "borrowable", "printdisabled", "no_ebook"].includes(b.access)) return undefined;
  if (b.year != null && (typeof b.year !== "number" || !Number.isSafeInteger(b.year) || b.year < 0 || b.year > 2200)) return undefined;
  if (b.editions != null && (typeof b.editions !== "number" || !Number.isSafeInteger(b.editions) || b.editions < 0)) return undefined;
  if (b.readingURL != null && (typeof b.readingURL !== "string" || b.readingURL.length > URL_MAX || !/^https:\/\/openlibrary\.org\/books\/ia:[A-Za-z0-9][A-Za-z0-9._%-]{0,150}$/.test(b.readingURL))) return undefined;
  try { workKey(b.key); } catch { return undefined; }
  return { key: b.key, title: clip(b.title, 500), authors: b.authors.slice(0, 8).map(x => clip(x as string, 200)),
    ...(b.year == null ? {} : { year: b.year as number }), ...(b.editions == null ? {} : { editions: b.editions as number }),
    access: b.access as Book["access"], url: b.url, ...(typeof b.readingURL === "string" ? { readingURL: b.readingURL } : {}) };
}
export function readState(raw: unknown): PageState {
  if (typeof raw !== "string" || byteLength(raw) > STATE_MAX) return initialState;
  try {
    const value = JSON.parse(raw) as Record<string, unknown>;
    if (!value || Array.isArray(value)) return initialState;
    const v: ViewKind = value.v === "search" || value.v === "work" ? value.v : "discover";
    const q = typeof value.q === "string" && value.q !== "" ? queryText(value.q) : "";
    const k = typeof value.k === "string" && value.k ? workKey(value.k) : "";
    const b = value.b === undefined ? undefined : book(value.b);
    if (value.b !== undefined && !b) return initialState;
    if (v === "work" && (!k || !b || k !== b.key) || v === "search" && !q) return initialState;
    return { v, q, k, ...(b === undefined ? {} : { b }) };
  } catch { return initialState; }
}
export const encodeState = (state: PageState) => {
  const encoded = JSON.stringify(state);
  if (byteLength(encoded) <= STATE_MAX) return encoded;
  const slim = JSON.stringify(state.q ? { v: "search", q: state.q, k: "" } : initialState);
  return byteLength(slim) <= STATE_MAX ? slim : JSON.stringify(initialState);
};
function safeState(state: PageState): PageState {
  try { return readState(JSON.stringify(state)); }
  catch { return initialState; }
}
export function transition(state: PageState, id: string, values: unknown = {}): PageState {
  const [kind, argument] = id.split(":", 2);
  if (kind === "work" && argument && state.b?.key === argument) return { ...state, v: "work", k: argument };
  let fields: Readonly<Record<string, unknown>>;
  if (typeof values === "string") {
    try { values = JSON.parse(values); } catch { return state; }
  }
  if (!values || typeof values !== "object" || Array.isArray(values)) return state;
  fields = values as Readonly<Record<string, unknown>>;
  const string = (key: string) => typeof fields[key] === "string" ? fields[key] as string : undefined;
  if (kind === "search") {
    const raw = string("query");
    if (raw === undefined || /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f\ud800-\udfff]/u.test(raw)) return state;
    try {
      const query = queryText(clip(plain(raw), 200));
      return { v: "search", q: query, k: "" };
    } catch { return state; }
  }
  if (kind === "back") return state.q ? { v: "search", q: state.q, k: "" } : initialState;
  if (kind === "home") return initialState;
  return state;
}
export async function loadPage(service: OpenLibrary, state: PageState, refresh: boolean, signal?: AbortSignal): Promise<Loaded> {
  const request = budget(signal, service.limits.page);
  try {
    if (state.v === "work") {
      if (!state.b) throw new ReadError("invalid");
      return { kind: "work", work: await service.work(state.k, state.b, refresh, request.signal) };
    }
    if (state.v === "search") return { kind: "search", result: await service.search(state.q, 20, refresh, request.signal) };
    return { kind: "empty" };
  } catch (error) { checkSignal(signal); return { kind: "error", error }; }
  finally { request.dispose(); }
}
export interface PageProps { service: OpenLibrary; state: PageState; loaded: Loaded; locale?: string; tz?: string; uiVersion?: number; action?: (id: string, next?: PageState, title?: string) => NativeAction | CallbackAction }
export function page(props: PageProps): Node {
  const { service, loaded, uiVersion = 0 } = props, state = safeState(props.state), stateJSON = encodeState(state), t = textsFor(props.locale ?? service.locale), tz = props.tz ?? "Local";
  const act = (id: string, next?: PageState, title?: string) => {
    const cleanTitle = title ? clip(title, TEXT_MAX) : undefined;
    return props.action?.(id, next, cleanTitle) ?? (next && uiVersion >= 3 ? { id, push: { ui: "openlibrary", state: encodeState(next), ...(cleanTitle ? { title: cleanTitle } : {}) } } : { id });
  };
  const text = (key: string, value: string, style: "body" | "caption" | "secondary" = "secondary") => <Text key={key} text={clip(value, TEXT_MAX)} style={style} />;
  const search = <Section key="book-search" title={clip(t.search, TEXT_MAX)}><Field key="book-query" name="query" label={clip(t.search, TEXT_MAX)} kind="text" placeholder={clip(t.searchHint, TEXT_MAX)} value={state.q} /><Button key="book-search-submit" title={clip(t.searchAction, TEXT_MAX)} action={act("search")} /></Section>;
  const navigation = <Section key="book-navigation"><Button key="book-back" title={clip(state.q ? t.back : t.backDiscover, TEXT_MAX)} action={act("back")} /></Section>;
  if (loaded.kind === "loading") return <View key="openlibrary" title={clip(t.discover, TEXT_MAX)} state={stateJSON}>{search}{state.v !== "discover" ? navigation : null}<Section key="book-loading"><Loading key="book-loading-indicator" title={clip(t.loading, TEXT_MAX)} /></Section></View>;
  if (loaded.kind === "error") return <View key="openlibrary" title={clip(t.discover, TEXT_MAX)} state={stateJSON}>{search}{state.v !== "discover" ? navigation : null}<Section key="book-error">{text("book-error-message", errorText(loaded.error, props.locale ?? service.locale))}<Button key="book-retry" title={clip(t.retry, TEXT_MAX)} action={act("refresh")} /></Section></View>;
  if (state.v === "work") {
    if (loaded.kind !== "work") throw new ReadError("invalid");
    const work = loaded.work;
    const readLink = safeLinkURL(work.readingURL), workLink = safeLinkURL(work.url);
    const readAt = readTime(work.at, tz, props.locale ?? service.locale);
    const footerText = clip(`${readAt ? fmt(t.readAt, { at: readAt }) : ""}${work.stale ? `${readAt ? " · " : ""}${errorText(work.stale, props.locale ?? service.locale)}` : ""}`, TEXT_MAX);
    return <View key="openlibrary" title={clip(work.title, TEXT_MAX)} state={stateJSON}>
      <Section key="work-details" title={clip(work.title, TEXT_MAX)} footer={clip([work.year ? fmt(t.year, { year: work.year }) : t.unknownYear, work.editions ? fmt(t.editions, { count: work.editions }) : "", t[work.access]].filter(Boolean).join(" · "), TEXT_MAX)}>
        {work.authors.length ? text("work-authors", work.authors.join(" · "), "body") : null}{text("work-description", work.description || t.noDescription, "body")}
      </Section>
      {work.subjects.length ? <Section key="work-subjects" title={clip(t.subjects, TEXT_MAX)}>{text("work-subject-list", work.subjects.join(" · "))}</Section> : null}
      <Section key="work-actions">{readLink ? <Link key="work-read" title={clip(t.read, TEXT_MAX)} url={readLink} /> : null}{workLink ? <Link key="work-open" title={clip(t.open, TEXT_MAX)} url={workLink} /> : null}<Button key="work-back" title={clip(t.back, TEXT_MAX)} action={act("back")} /></Section>
      <Section key="work-footer" footer={footerText}><Button key="work-refresh" title={clip(t.refresh, TEXT_MAX)} action={act("refresh")} /></Section>
      <Section key="work-attribution">{text("work-license", t.attribution, "caption")}</Section>
    </View>;
  }
  const usedKeys = new Set(["book-search", "book-query", "book-search-submit", "book-results", "book-empty", "book-home", "book-welcome", "book-welcome-text", "book-attribution", "book-license"]);
  const reservedItems = 2 + 1 + 1;
  let remainingItems = Math.max(0, ITEMS_MAX - reservedItems);
  let remainingBytes = FRAME_MAX - byteLength(stateJSON) - 8192;
  const rows: Node[] = [];
  if (state.v === "search" && loaded.kind === "search") {
    for (const item of loaded.result.books) {
      if (remainingItems < 1) break;
      const rowKey = uniqueKey(usedKeys, "book-", item.key);
      const title = clip(item.title, TEXT_MAX);
      const subtitle = clip([item.authors.join(" · "), item.year ? fmt(t.year, { year: item.year }) : "", t[item.access]].filter(Boolean).join(" · "), TEXT_MAX);
      const nextState: PageState = { v: "work", q: state.q, k: item.key, b: item };
      const nextStateJSON = uiVersion >= 3 ? encodeState(nextState) : "";
      const estimatedBytes = byteLength(rowKey) + byteLength(title) + byteLength(subtitle) + byteLength(nextStateJSON) + 384;
      if (estimatedBytes > remainingBytes) break;
      remainingItems -= 1;
      remainingBytes -= estimatedBytes;
      rows.push(<Row key={rowKey} title={title} subtitle={subtitle} action={act(`work:${item.key}`, nextState, title)} />);
    }
  }
  return <View key="openlibrary" title={clip(state.v === "search" ? fmt(t.results, { query: clip(state.q, 100) }) : t.discover, TEXT_MAX)} state={stateJSON}>{search}
    {state.v === "search" ? <Section key="book-results" title={clip(fmt(t.results, { query: clip(state.q, 100) }), TEXT_MAX)}>
      {rows.length ? rows : text("book-empty", t.empty)}<Button key="book-home" title={clip(t.backDiscover, TEXT_MAX)} action={act("home")} />
    </Section> : <Section key="book-welcome">{text("book-welcome-text", t.searchHint, "body")}</Section>}
    <Section key="book-attribution">{text("book-license", t.attribution, "caption")}</Section>
  </View>;
}
