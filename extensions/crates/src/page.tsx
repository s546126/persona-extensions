import { Button, Field, Link, Loading, Section, Text, View, type CallbackAction, type NativeAction, type Node } from "@persona/extension/ui";
import { clip, crateName, queryText, ReadError, type CrateSummary, type DetailResult, type SearchResult } from "./model.ts";
import { dateText, errorText, fmt, number, textsFor } from "./i18n.ts";
import { budget, checkSignal, type Crates } from "./service.ts";
export interface PageState { q: string; found: boolean; crate?: CrateSummary }
export type Loaded = { kind: "loading" } | { kind: "empty" } | { kind: "results"; result: SearchResult } | { kind: "detail"; result: DetailResult } | { kind: "error"; error: unknown };
export const initialState: PageState = { q: "", found: false };
const MAX_VIEW_ITEMS = 300;
const MAX_RESULT_CHROME_ITEMS = 8;
const MAX_RESULT_ITEMS = MAX_VIEW_ITEMS - MAX_RESULT_CHROME_ITEMS;
const MAX_RESULT_BYTES = 512 * 1024;
const MAX_STATE_BYTES = 16 * 1024;
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
    return url.protocol === "https:" && url.hostname && !url.username && !url.password && !url.port ? url.href : undefined;
  } catch { return undefined; }
}
function summaryState(value: unknown): CrateSummary | undefined {
  if (value == null) return undefined;
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new ReadError("invalid");
  const raw = value as Record<string, unknown>, name = crateName(raw.name);
  const rawDesc = typeof raw.description === "string" ? raw.description : "";
  const rawLicense = typeof raw.license === "string" ? raw.license : "";
  if (/[\ud800-\udfff]/u.test(rawDesc) || /[\u0000-\u001f\u007f\ud800-\udfff]/u.test(rawLicense)) throw new ReadError("invalid");
  const summary: CrateSummary = { name, version: typeof raw.version === "string" && /^[0-9A-Za-z][0-9A-Za-z.+-]{0,99}$/.test(raw.version) ? raw.version : (() => { throw new ReadError("invalid"); })(), description: clip(rawDesc, 500), license: clip(rawLicense, 120), url: `https://crates.io/crates/${name}` };
  for (const field of ["repository", "homepage", "documentation"] as const) {
    if (raw[field] == null) continue;
    if (typeof raw[field] !== "string") throw new ReadError("invalid");
    const link = new URL(raw[field]);
    if (link.protocol !== "https:" || link.username || link.password || link.port || link.href.length > 4096) throw new ReadError("invalid");
    summary[field] = link.href;
  }
  if (raw.updated != null) {
    if (typeof raw.updated !== "string" || !Number.isFinite(Date.parse(raw.updated))) throw new ReadError("invalid");
    summary.updated = raw.updated;
  }
  for (const field of ["recentDownloads", "totalDownloads"] as const) {
    if (raw[field] == null) continue;
    if (typeof raw[field] !== "number" || !Number.isSafeInteger(raw[field]) || (raw[field] as number) < 0 || (raw[field] as number) > 2_000_000_000_000) throw new ReadError("invalid");
    summary[field] = raw[field] as number;
  }
  return summary;
}
export function readState(raw: unknown): PageState {
  if (typeof raw !== "string" || raw.length > 10_000) return initialState;
  try {
    const value = JSON.parse(raw) as Record<string, unknown>;
    if (!value || Array.isArray(value) || typeof value.found !== "boolean") return initialState;
    return { q: queryText(value.q), found: value.found, crate: summaryState(value.crate) };
  } catch { return initialState; }
}
export const encodeState = (state: PageState) => JSON.stringify(state);
function boundState(state: PageState): PageState {
  const base: PageState = { q: clip(state.q, 100), found: Boolean(state.found) };
  if (!state.crate) return base;
  const candidate: PageState = { ...base, crate: { ...state.crate, description: clip(state.crate.description, 500), license: clip(state.crate.license, 120) } };
  return encoder.encode(encodeState(candidate)).byteLength <= MAX_STATE_BYTES ? candidate : base;
}
export function crateTransition(state: PageState, result: SearchResult, id: string): PageState {
  if (!id.startsWith("crate:")) return state;
  const crate = result.crates.find(candidate => id === `crate:${candidate.name}`);
  return crate ? { q: state.q, found: true, crate } : state;
}
export function transition(state: PageState, id: string, values: unknown = {}): PageState {
  let fields: Readonly<Record<string, unknown>>;
  if (typeof values === "string") { try { values = JSON.parse(values); } catch { return state; } }
  if (!values || typeof values !== "object" || Array.isArray(values)) return state;
  fields = values as Readonly<Record<string, unknown>>;
  if (id === "search") {
    try {
      const next = { q: queryText(fields.query), found: true };
      return next.q === state.q && state.found ? { ...next, crate: state.crate } : next;
    } catch { return state; }
  }
  if (id === "home") return initialState;
  if (id === "back" && state.found) return { q: state.q, found: true };
  return state;
}
export async function loadPage(service: Crates, state: PageState, refresh: boolean, signal?: AbortSignal): Promise<Loaded> {
  const request = budget(signal, service.limits.page);
  try {
    if (!state.found) return { kind: "empty" };
    if (state.crate) return { kind: "detail", result: await service.crate(state.crate.name, refresh, request.signal) };
    return { kind: "results", result: await service.search(state.q, 12, refresh, request.signal) };
  } catch (error) {
    checkSignal(signal);
    return { kind: "error", error: !(error instanceof ReadError) && request.signal.aborted && request.signal.reason instanceof ReadError ? request.signal.reason : error };
  }
  finally { request.dispose(); }
}
export interface PageProps { service: Crates; state: PageState; loaded: Loaded; locale?: string; uiVersion?: number; action?: (id: string, next?: PageState, title?: string) => NativeAction | CallbackAction }
export function page(props: PageProps): Node {
  const { service, state, loaded, uiVersion = 0 } = props, locale = props.locale || service.locale, t = textsFor(locale);
  const safeState = boundState(state);
  const encodedState = encodeState(safeState);
  const act = (id: string, next?: PageState, title?: string) => {
    const safeNext = next ? boundState(next) : undefined;
    const safeTitle = title ? clip(title, 2000) : undefined;
    return props.action?.(id, safeNext, safeTitle) ?? (safeNext && uiVersion >= 3 ? { id, push: { ui: "crates", state: encodeState(safeNext), ...(safeTitle ? { title: safeTitle } : {}) } } : { id });
  };
  const text = (key: string, value: string, style: "body" | "caption" | "secondary" = "secondary") => <Text key={key} text={clip(value || t.unknown, 2000)} style={style} />;
  const search = <Section key="crates-search" title={t.search}><Field key="crates-query" name="query" label={t.search} kind="text" placeholder={t.searchHint} value={safeState.q} /><Button key="crates-submit" title={t.searchAction} action={act("search")} /></Section>;
  const attribution = <Section key="crates-attribution">{text("crates-license", t.attribution, "caption")}</Section>;
  const footer = <Section key="crates-footer"><Button key="crates-refresh" title={t.retry} action={act("refresh")} />{safeState.crate ? <Button key="crates-back" title={t.back} action={act("back")} /> : null}<Button key="crates-home" title={t.home} action={act("home")} /></Section>;
  const stale = (error?: ReadError) => error ? <Section key="crates-stale">{text("crates-stale-text", errorText(error, locale))}</Section> : null;
  if (loaded.kind === "loading") return <View key="crates" title={t.search} state={encodedState}>{search}<Section key="crates-loading"><Loading key="crates-loading-indicator" title={t.loading} /></Section>{attribution}</View>;
  if (loaded.kind === "error") return <View key="crates" title={t.search} state={encodedState}>{search}<Section key="crates-error">{text("crates-error-message", errorText(loaded.error, locale))}<Button key="crates-retry" title={t.retry} action={act("refresh")} /></Section>{attribution}</View>;
  if (loaded.kind === "empty") return <View key="crates" title={t.search} state={encodedState}>{search}<Section key="crates-welcome">{text("crates-welcome-text", t.welcome, "body")}</Section>{attribution}</View>;
  if (loaded.kind === "detail") {
    const crate = loaded.result.crate;
    const publishedText = crate.published ? dateText(crate.published) : "";
    const detailFooter = clip([fmt(t.license, { license: crate.license }), publishedText ? fmt(t.published, { version: crate.version, date: publishedText }) : `v${crate.version}`].join(" · "), 2000);
    const docsURL = validHTTPS(crate.documentation);
    const repoURL = validHTTPS(crate.repository);
    const homeURL = validHTTPS(crate.homepage);
    const crateURL = validHTTPS(crate.url) ?? `https://crates.io/crates/${crate.name}`;
    return <View key="crates" title={clip(crate.name, 2000)} state={encodedState}>{search}
      <Section key="crates-detail" title={clip(crate.name, 2000)} footer={detailFooter}>
        {text("crates-description", crate.description, "body")}
        {crate.recentDownloads !== undefined ? text("crates-recent", fmt(t.recent, { count: number(crate.recentDownloads) })) : null}
        {crate.totalDownloads !== undefined ? text("crates-total", fmt(t.total, { count: number(crate.totalDownloads) })) : null}
        {crate.versionDownloads !== undefined ? text("crates-version-downloads", fmt(t.versionDownloads, { count: number(crate.versionDownloads) })) : null}
        {crate.yanked ? text("crates-yanked", t.yanked, "caption") : null}
      </Section>
      <Section key="crates-install" title={t.install}>{text("crates-install-command", crate.install, "body")}{text("crates-install-hint", t.installHint, "caption")}</Section>
      <Section key="crates-links"><Link key="crates-crate-link" title={t.openCrate} url={crateURL} />{docsURL ? <Link key="crates-docs-link" title={t.openDocs} url={docsURL} /> : null}{repoURL ? <Link key="crates-repository-link" title={t.openRepository} url={repoURL} /> : null}{homeURL ? <Link key="crates-homepage-link" title={t.openHomepage} url={homeURL} /> : null}</Section>
      {stale(loaded.result.stale)}{footer}{attribution}
    </View>;
  }
  const cards: Node[] = [];
  const usedKeys = new Set<string>();
  const uniqueID = (prefix: string, value: string): string => {
    let key = boundedID(prefix, value);
    for (let n = 1; usedKeys.has(key); n++) key = boundedID(prefix, `${value}~${n}`);
    usedKeys.add(key);
    return key;
  };
  let remainingItems = MAX_RESULT_ITEMS;
  let remainingBytes = MAX_RESULT_BYTES;
  for (const crate of loaded.result.crates) {
    if (remainingItems < 3) break;
    const title = clip(`${crate.name}@${crate.version}`, 2000);
    const updatedText = crate.updated ? dateText(crate.updated) : "";
    const cardFooter = clip([fmt(t.license, { license: crate.license }), crate.recentDownloads !== undefined ? fmt(t.recent, { count: number(crate.recentDownloads) }) : "", updatedText].filter(Boolean).join(" · "), 2000);
    const desc = clip(crate.description || t.unknown, 2000);
    const crateURL = validHTTPS(crate.url) ?? `https://crates.io/crates/${crate.name}`;
    const sectionKey = uniqueID("crate-", crate.name);
    const descKey = uniqueID("description-", crate.name);
    const openKey = uniqueID("open-", crate.name);
    const linkKey = uniqueID("link-", crate.name);
    const nextState = boundState({ ...safeState, crate });
    const cardBytes = encoder.encode(JSON.stringify([sectionKey, title, cardFooter, descKey, desc, openKey, linkKey, crateURL, nextState])).byteLength;
    if (cardBytes > remainingBytes) break;
    cards.push(<Section key={sectionKey} title={title} footer={cardFooter}>
      {text(descKey, desc, "body")}
      <Button key={openKey} title={t.details} action={act(`crate:${crate.name}`, nextState, crate.name)} />
      <Link key={linkKey} title={t.openCrate} url={crateURL} />
    </Section>);
    remainingItems -= 3;
    remainingBytes -= cardBytes;
  }
  return <View key="crates" title={clip(`${t.title}: ${clip(safeState.q, 100)}`, 2000)} state={encodedState}>{search}
    <Section key="crates-result-count">{text("crates-total-results", fmt(t.results, { total: number(loaded.result.total) }), "caption")}</Section>
    {cards.length ? cards : <Section key="crates-empty-results">{text("crates-empty", t.empty)}</Section>}
    {stale(loaded.result.stale)}{footer}{attribution}
  </View>;
}
