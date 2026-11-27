import { Button, Field, Image, Link, Loading, Section, Text, View, type CallbackAction, type NativeAction, type Node } from "@persona/extension/ui";
import { clip, queryText, ReadError, type SearchResult } from "./model.ts";
import { errorText, fmt, readTime, textsFor } from "./i18n.ts";
import { budget, checkSignal, type Commons } from "./service.ts";
export interface PageState { q: string; found: boolean }
export type Loaded = { kind: "loading" } | { kind: "empty" } | { kind: "result"; result: SearchResult } | { kind: "error"; error: unknown };
export const initialState: PageState = { q: "", found: false };
const MAX_VIEW_ITEMS = 300;
const RESULT_CHROME_ITEMS = 5;
const MAX_RESULT_ITEMS = MAX_VIEW_ITEMS - RESULT_CHROME_ITEMS;
const MAX_VIEW_IMAGES = 50;
const MAX_RESULT_BYTES = 512 * 1024;
const encoder = new TextEncoder();
function shortHash(value: string): string {
  let hash = 0x811c9dc5;
  for (let i = 0; i < value.length; i++) {
    hash ^= value.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193);
  }
  return (hash >>> 0).toString(16).padStart(8, "0");
}
function boundedID(prefix: string, value: string, max = 128): string {
  const full = prefix + value;
  if (Array.from(full).length <= max) return full;
  const suffix = "~" + shortHash(value);
  const keep = max - Array.from(prefix).length - suffix.length;
  return prefix + Array.from(value).slice(0, Math.max(0, keep)).join("") + suffix;
}
function validHTTPS(raw: string | undefined, maxLen = 4096): string | undefined {
  if (!raw || raw.length > maxLen) return undefined;
  try {
    const url = new URL(raw);
    return url.protocol === "https:" && url.hostname && !url.username && !url.password ? url.href : undefined;
  } catch { return undefined; }
}
function validDimension(value: number | undefined): value is number {
  return value !== undefined && Number.isSafeInteger(value) && value >= 1 && value <= 8192;
}
export function readState(raw: unknown): PageState {
  if (typeof raw !== "string" || raw.length > 2000) return initialState;
  try {
    const value = JSON.parse(raw) as Record<string, unknown>;
    if (!value || Array.isArray(value) || typeof value.q !== "string" || typeof value.found !== "boolean") return initialState;
    if (!value.q.trim()) return initialState;
    return { q: queryText(value.q), found: value.found };
  } catch { return initialState; }
}
export const encodeState = (state: PageState) => JSON.stringify(state);
export function transition(state: PageState, id: string, values: unknown = {}): PageState {
  let fields: Readonly<Record<string, unknown>>;
  if (typeof values === "string") {
    try { values = JSON.parse(values); } catch { return state; }
  }
  if (!values || typeof values !== "object" || Array.isArray(values)) return state;
  fields = values as Readonly<Record<string, unknown>>;
  if (id === "search") {
    if (typeof fields.query !== "string") return state;
    try { return { q: queryText(fields.query), found: true }; } catch { return state; }
  }
  if (id === "home") return initialState;
  return state;
}
export async function loadPage(service: Commons, state: PageState, refresh: boolean, signal?: AbortSignal): Promise<Loaded> {
  const request = budget(signal, service.limits.page);
  try {
    if (!state.found) return { kind: "empty" };
    return { kind: "result", result: await service.search(state.q, 20, refresh, request.signal) };
  } catch (error) {
    checkSignal(signal);
    return { kind: "error", error: !(error instanceof ReadError) && request.signal.aborted && request.signal.reason instanceof ReadError ? request.signal.reason : error };
  }
  finally { request.dispose(); }
}
export interface PageProps { service: Commons; state: PageState; loaded: Loaded; locale?: string; tz?: string; uiVersion?: number; action?: (id: string, next?: PageState, title?: string) => NativeAction | CallbackAction }
export function page(props: PageProps): Node {
  const { service, state, loaded, uiVersion = 0 } = props, locale = props.locale || service.locale, t = textsFor(locale), tz = props.tz || "Local";
  const safeState: PageState = { q: clip(state.q, 160), found: Boolean(state.found) };
  const encodedState = encodeState(safeState);
  const act = (id: string, next?: PageState, title?: string) => props.action?.(id, next, title) ?? (next && uiVersion >= 3 ? { id, push: { ui: "search", state: encodeState(next), ...(title ? { title } : {}) } } : { id });
  const text = (key: string, value: string, style: "body" | "caption" | "secondary" = "secondary") => <Text key={key} text={clip(value, 2000)} style={style} />;
  const search = <Section key="commons-search" title={t.search}><Field key="commons-query" name="query" label={t.search} kind="text" placeholder={t.searchHint} value={safeState.q} /><Button key="commons-submit" title={t.searchAction} action={act("search")} /></Section>;
  if (loaded.kind === "loading") return <View key="commons" title={t.search} state={encodedState}>{search}<Section key="commons-loading"><Loading key="commons-loading-indicator" title={t.loading} /></Section></View>;
  if (loaded.kind === "error") return <View key="commons" title={t.search} state={encodedState}>{search}<Section key="commons-error">{text("commons-error-message", errorText(loaded.error, locale))}<Button key="commons-retry" title={t.retry} action={act("refresh")} /></Section></View>;
  if (loaded.kind === "empty") return <View key="commons" title={t.search} state={encodedState}>{search}<Section key="commons-welcome">{text("commons-welcome-text", t.welcome, "body")}</Section><Section key="commons-attribution">{text("commons-license", t.attribution, "caption")}</Section></View>;
  const result = loaded.result;
  const cards: Node[] = [];
  const usedKeys = new Set<string>();
  const uniqueID = (prefix: string, value: string): string => {
    let key = boundedID(prefix, value);
    for (let n = 1; usedKeys.has(key); n++) key = boundedID(prefix, `${value}~${n}`);
    usedKeys.add(key);
    return key;
  };
  let remainingItems = MAX_RESULT_ITEMS;
  let remainingImages = MAX_VIEW_IMAGES;
  let remainingBytes = MAX_RESULT_BYTES;
  for (const image of result.images) {
    const idStr = String(image.id);
    const thumbURL = validHTTPS(image.thumbnailURL, 4096);
    const showImage = Boolean(thumbURL && remainingImages > 0);
    const pageURL = validHTTPS(image.pageURL, 4096);
    const licenseURL = validHTTPS(image.licenseURL, 4096);
    const cardItems = 2 + (showImage ? 1 : 0) + (pageURL ? 1 : 0) + (licenseURL ? 1 : 0);
    if (cardItems > remainingItems) break;
    const title = clip(image.title, 2000);
    const artistText = clip(`${t.artist}: ${image.artist || t.noDescription}`, 2000);
    const licenseText = clip(`${t.license}: ${image.license || t.licenseUnknown}`, 2000);
    const alt = clip(image.description || image.title || t.search, 200);
    const caption = clip(image.description || t.noDescription, 1000);
    const bothDims = image.width !== undefined && image.height !== undefined;
    const dims = bothDims
      ? (validDimension(image.width) && validDimension(image.height) ? { width: image.width, height: image.height } : {})
      : { ...(validDimension(image.width) ? { width: image.width } : {}), ...(validDimension(image.height) ? { height: image.height } : {}) };
    const sectionKey = uniqueID("image-card-", idStr);
    const pictureKey = showImage ? uniqueID("picture-", idStr) : "";
    const artistKey = uniqueID("artist-", idStr);
    const licenseKey = uniqueID("license-", idStr);
    const pageKey = pageURL ? uniqueID("page-", idStr) : "";
    const licenseLinkKey = licenseURL ? uniqueID("license-link-", idStr) : "";
    const cardBytes = encoder.encode(JSON.stringify([sectionKey, title, pictureKey, thumbURL, alt, caption, artistKey, artistText, licenseKey, licenseText, pageKey, pageURL, licenseLinkKey, licenseURL])).byteLength;
    if (cardBytes > remainingBytes) break;
    cards.push(<Section key={sectionKey} title={title}>
      {showImage && thumbURL ? <Image key={pictureKey} src={{ url: thumbURL, ...dims }} alt={alt} caption={caption} aspect="landscape" /> : null}
      {text(artistKey, artistText, "body")}
      {text(licenseKey, licenseText)}
      {pageURL ? <Link key={pageKey} title={t.open} url={pageURL} /> : null}
      {licenseURL ? <Link key={licenseLinkKey} title={t.licenseLink} url={licenseURL} /> : null}
    </Section>);
    remainingItems -= cardItems;
    if (showImage) remainingImages -= 1;
    remainingBytes -= cardBytes;
  }
  const footer = clip(`${fmt(t.readAt, { at: readTime(result.at, tz, locale) })}${result.stale ? ` · ${errorText(result.stale, locale)}` : ""}`, 2000);
  return <View key="commons" title={clip(fmt(t.results, { query: clip(safeState.q, 100) }), 2000)} state={encodedState}>{search}
    {cards.length ? cards : <Section key="commons-empty">{text("commons-empty-text", t.empty)}</Section>}
    <Section key="commons-footer" footer={footer}><Button key="commons-refresh" title={t.retry} action={act("refresh")} /><Button key="commons-home" title={t.search} action={act("home")} /></Section>
    <Section key="commons-attribution">{text("commons-license", t.attribution, "caption")}</Section>
  </View>;
}
