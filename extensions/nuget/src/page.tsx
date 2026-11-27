import { Button, Field, Link, Loading, Section, Text, View, type CallbackAction, type NativeAction, type Node } from "@persona/extension/ui";
import { boundedID, clip, packageActionID, packageID, queryText, ReadError, type DetailResult, type PackageSummary, type SearchResult } from "./model.ts";
import { dateText, errorText, fmt, number, textsFor } from "./i18n.ts";
import { budget, checkSignal, type Nuget } from "./service.ts";
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
export interface PageState { q: string; found: boolean; package?: PackageSummary }
export type Loaded = { kind: "loading" } | { kind: "empty" } | { kind: "results"; result: SearchResult } | { kind: "detail"; result: DetailResult } | { kind: "error"; error: unknown };
export const initialState: PageState = { q: "", found: false };
function summaryState(value: unknown): PackageSummary | undefined {
  if (value == null) return undefined;
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new ReadError("invalid");
  const raw = value as Record<string, unknown>, id = packageID(raw.id);
  const version = typeof raw.version === "string" && /^[0-9A-Za-z][0-9A-Za-z.+-]{0,99}$/.test(raw.version) ? raw.version : (() => { throw new ReadError("invalid"); })();
  if (raw.description != null && (typeof raw.description !== "string" || /[\ud800-\udfff]/u.test(raw.description))) throw new ReadError("invalid");
  if (raw.license != null && (typeof raw.license !== "string" || /[\u0000-\u001f\u007f\ud800-\udfff]/u.test(raw.license))) throw new ReadError("invalid");
  const summary: PackageSummary = { id, version, description: clip(typeof raw.description === "string" ? raw.description : "", 500), license: clip(typeof raw.license === "string" ? raw.license : "", 120), url: `https://www.nuget.org/packages/${id}/${version}` };
  for (const field of ["projectURL", "licenseURL"] as const) {
    if (raw[field] == null) continue;
    const link = safeLinkURL(raw[field]);
    if (!link) throw new ReadError("invalid");
    summary[field] = link;
  }
  if (raw.published != null) {
    if (typeof raw.published !== "string" || !Number.isFinite(Date.parse(raw.published))) throw new ReadError("invalid");
    summary.published = raw.published;
  }
  if (raw.totalDownloads != null) {
    if (typeof raw.totalDownloads !== "number" || !Number.isSafeInteger(raw.totalDownloads) || raw.totalDownloads < 0 || raw.totalDownloads > 20_000_000_000_000) throw new ReadError("invalid");
    summary.totalDownloads = raw.totalDownloads;
  }
  return summary;
}
export function readState(raw: unknown): PageState {
  if (typeof raw !== "string" || byteLength(raw) > STATE_MAX) return initialState;
  try {
    const value = JSON.parse(raw) as Record<string, unknown>;
    if (!value || Array.isArray(value) || typeof value.found !== "boolean") return initialState;
    if (!value.found && (value.q === "" || value.q == null) && value.package == null) return initialState;
    return { q: queryText(value.q), found: value.found, ...(value.package == null ? {} : { package: summaryState(value.package) }) };
  } catch { return initialState; }
}
export const encodeState = (state: PageState) => {
  const encoded = JSON.stringify(state);
  if (byteLength(encoded) <= STATE_MAX) return encoded;
  const slim = JSON.stringify({ q: state.q, found: state.found });
  return byteLength(slim) <= STATE_MAX ? slim : JSON.stringify(initialState);
};
function safeState(state: PageState): PageState {
  try { return readState(JSON.stringify(state)); }
  catch { return initialState; }
}
export function packageTransition(state: PageState, result: SearchResult, id: string): PageState {
  if (!id.startsWith("package:")) return state;
  const pkg = result.packages.find(candidate => id === packageActionID(candidate.id) || id === `package:${candidate.id}`);
  return pkg ? { q: state.q, found: true, package: pkg } : state;
}
export function transition(state: PageState, id: string, values: unknown = {}): PageState {
  let fields: Readonly<Record<string, unknown>>;
  if (typeof values === "string") { try { values = JSON.parse(values); } catch { return state; } }
  if (!values || typeof values !== "object" || Array.isArray(values)) return state;
  fields = values as Readonly<Record<string, unknown>>;
  if (id === "search") {
    try {
      const next = { q: queryText(fields.query), found: true };
      return next.q === state.q && state.found ? { ...next, package: state.package } : next;
    } catch { return state; }
  }
  if (id === "home") return initialState;
  if (id === "back" && state.found) return { q: state.q, found: true };
  return state;
}
export async function loadPage(service: Nuget, state: PageState, refresh: boolean, signal?: AbortSignal): Promise<Loaded> {
  const request = budget(signal, service.limits.page);
  try {
    if (!state.found) return { kind: "empty" };
    if (state.package) return { kind: "detail", result: await service.package(state.package.id, refresh, request.signal) };
    return { kind: "results", result: await service.search(state.q, 12, refresh, request.signal) };
  } catch (error) { checkSignal(signal); return { kind: "error", error }; }
  finally { request.dispose(); }
}
export interface PageProps { service: Nuget; state: PageState; loaded: Loaded; locale?: string; uiVersion?: number; action?: (id: string, next?: PageState, title?: string) => NativeAction | CallbackAction }
export function page(props: PageProps): Node {
  const { service, loaded, uiVersion = 0 } = props, state = safeState(props.state), stateJSON = encodeState(state), t = textsFor(props.locale ?? service.locale);
  const act = (id: string, next?: PageState, title?: string) => {
    const cleanTitle = title ? clip(title, TEXT_MAX) : undefined;
    return props.action?.(id, next, cleanTitle) ?? (next && uiVersion >= 3 ? { id, push: { ui: "nuget", state: encodeState(next), ...(cleanTitle ? { title: cleanTitle } : {}) } } : { id });
  };
  const text = (key: string, value: string, style: "body" | "caption" | "secondary" = "secondary") => <Text key={key} text={clip(value || t.unknown, TEXT_MAX)} style={style} />;
  const search = <Section key="nuget-search" title={clip(t.search, TEXT_MAX)}><Field key="nuget-query" name="query" label={clip(t.search, TEXT_MAX)} kind="text" placeholder={clip(t.searchHint, TEXT_MAX)} value={state.q} /><Button key="nuget-submit" title={clip(t.searchAction, TEXT_MAX)} action={act("search")} /></Section>;
  const attribution = <Section key="nuget-attribution">{text("nuget-license", t.attribution, "caption")}</Section>;
  const footer = <Section key="nuget-footer"><Button key="nuget-refresh" title={clip(t.retry, TEXT_MAX)} action={act("refresh")} />{state.package ? <Button key="nuget-back" title={clip(t.back, TEXT_MAX)} action={act("back")} /> : null}<Button key="nuget-home" title={clip(t.home, TEXT_MAX)} action={act("home")} /></Section>;
  const stale = (error?: ReadError) => error ? <Section key="nuget-stale">{text("nuget-stale-text", errorText(error, props.locale ?? service.locale))}</Section> : null;
  if (loaded.kind === "loading") return <View key="nuget" title={clip(t.search, TEXT_MAX)} state={stateJSON}>{search}<Section key="nuget-loading"><Loading key="nuget-loading-indicator" title={clip(t.loading, TEXT_MAX)} /></Section>{attribution}</View>;
  if (loaded.kind === "error") return <View key="nuget" title={clip(t.search, TEXT_MAX)} state={stateJSON}>{search}<Section key="nuget-error">{text("nuget-error-message", errorText(loaded.error, props.locale ?? service.locale))}<Button key="nuget-retry" title={clip(t.retry, TEXT_MAX)} action={act("refresh")} /></Section>{attribution}</View>;
  if (loaded.kind === "empty") return <View key="nuget" title={clip(t.search, TEXT_MAX)} state={stateJSON}>{search}<Section key="nuget-welcome">{text("nuget-welcome-text", t.welcome, "body")}</Section>{attribution}</View>;
  if (loaded.kind === "detail") {
    const pkg = loaded.result.package;
    const pkgURL = safeLinkURL(pkg.url), projectURL = safeLinkURL(pkg.projectURL), licenseURL = safeLinkURL(pkg.licenseURL);
    const hasLinks = Boolean(pkgURL || projectURL || licenseURL);
    return <View key="nuget" title={clip(pkg.id, TEXT_MAX)} state={stateJSON}>{search}
      <Section key="nuget-detail" title={clip(pkg.id, TEXT_MAX)} footer={clip([fmt(t.license, { license: pkg.license }), pkg.published ? fmt(t.published, { version: pkg.version, date: dateText(pkg.published) }) : `v${pkg.version}`].join(" · "), TEXT_MAX)}>
        {text("nuget-description", pkg.description, "body")}
        {pkg.totalDownloads !== undefined ? text("nuget-downloads", fmt(t.downloads, { count: number(pkg.totalDownloads) })) : null}
        {text("nuget-dependencies", fmt(t.dependencies, { count: pkg.dependencies }))}
        {text("nuget-frameworks", `${t.frameworks}: ${pkg.frameworks.join(" · ")}`)}
        {!pkg.listed ? text("nuget-unlisted", t.listed, "caption") : null}
      </Section>
      <Section key="nuget-install" title={clip(t.install, TEXT_MAX)}>{text("nuget-install-command", pkg.install, "body")}{text("nuget-install-hint", t.installHint, "caption")}</Section>
      {hasLinks ? <Section key="nuget-links">{pkgURL ? <Link key="nuget-package-link" title={clip(t.openPackage, TEXT_MAX)} url={pkgURL} /> : null}{projectURL ? <Link key="nuget-project-link" title={clip(t.openProject, TEXT_MAX)} url={projectURL} /> : null}{licenseURL ? <Link key="nuget-license-link" title={clip(t.openLicense, TEXT_MAX)} url={licenseURL} /> : null}</Section> : null}
      {stale(loaded.result.stale)}{footer}{attribution}
    </View>;
  }
  const usedKeys = new Set(["nuget-search", "nuget-query", "nuget-submit", "nuget-result-count", "nuget-total-results", "nuget-empty-results", "nuget-empty", "nuget-stale", "nuget-stale-text", "nuget-footer", "nuget-refresh", "nuget-back", "nuget-home", "nuget-attribution", "nuget-license"]);
  const reservedItems = 2 + 1 + (loaded.result.stale ? 1 : 0) + (state.package ? 3 : 2) + 1;
  let remainingItems = Math.max(0, ITEMS_MAX - reservedItems);
  let remainingBytes = FRAME_MAX - byteLength(stateJSON) - 8192;
  const cards: Node[] = [];
  for (const pkg of loaded.result.packages) {
    const pkgURL = safeLinkURL(pkg.url);
    const neededItems = pkgURL ? 3 : 2;
    if (neededItems > remainingItems) break;
    const sectionKey = uniqueKey(usedKeys, "package-", pkg.id);
    const descKey = uniqueKey(usedKeys, "description-", pkg.id);
    const openKey = uniqueKey(usedKeys, "open-", pkg.id);
    const linkKey = pkgURL ? uniqueKey(usedKeys, "link-", pkg.id) : "";
    const title = clip(`${pkg.id}@${pkg.version}`, TEXT_MAX);
    const cardFooter = clip([fmt(t.license, { license: pkg.license }), pkg.totalDownloads !== undefined ? fmt(t.downloads, { count: number(pkg.totalDownloads) }) : "", pkg.published ? dateText(pkg.published) : ""].filter(Boolean).join(" · "), TEXT_MAX);
    const description = clip(pkg.description || t.unknown, TEXT_MAX);
    const nextState: PageState = { ...state, package: pkg };
    const nextStateJSON = uiVersion >= 3 ? encodeState(nextState) : "";
    const actionID = packageActionID(pkg.id);
    const estimatedBytes = byteLength(sectionKey) + byteLength(descKey) + byteLength(openKey) + byteLength(linkKey) + byteLength(title) + byteLength(cardFooter) + byteLength(description) + byteLength(nextStateJSON) + byteLength(actionID) + (pkgURL ? byteLength(pkgURL) : 0) + 512;
    if (estimatedBytes > remainingBytes) break;
    remainingItems -= neededItems;
    remainingBytes -= estimatedBytes;
    cards.push(<Section key={sectionKey} title={title} footer={cardFooter}>
      {text(descKey, description, "body")}
      <Button key={openKey} title={clip(t.details, TEXT_MAX)} action={act(actionID, nextState, pkg.id)} />
      {pkgURL ? <Link key={linkKey} title={clip(t.openPackage, TEXT_MAX)} url={pkgURL} /> : null}
    </Section>);
  }
  return <View key="nuget" title={clip(`${t.title}: ${clip(state.q, 100)}`, TEXT_MAX)} state={stateJSON}>{search}
    <Section key="nuget-result-count">{text("nuget-total-results", fmt(t.results, { total: number(loaded.result.total) }), "caption")}</Section>
    {cards.length ? cards : <Section key="nuget-empty-results">{text("nuget-empty", t.empty)}</Section>}
    {stale(loaded.result.stale)}{footer}{attribution}
  </View>;
}
