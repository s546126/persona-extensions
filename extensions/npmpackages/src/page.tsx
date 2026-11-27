import { Button, Field, Link, Loading, Section, Text, View, type CallbackAction, type NativeAction, type Node } from "@persona/extension/ui";
import { clip, packageName, queryText, ReadError, type DetailResult, type PackageSummary, type SearchResult } from "./model.ts";
import { bytes, dateText, errorText, fmt, number, textsFor } from "./i18n.ts";
import { budget, checkSignal, type NpmPackages } from "./service.ts";
export interface PageState { q: string; found: boolean; package?: PackageSummary }
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
export const packageActionID = (name: string) => boundedID("package:", name, 128);
function validHTTPS(raw: string | undefined, maxLen = 4096): string | undefined {
  if (!raw || raw.length > maxLen) return undefined;
  try {
    const url = new URL(raw);
    return url.protocol === "https:" && url.hostname && !url.username && !url.password && !url.port ? url.href : undefined;
  } catch { return undefined; }
}
function summaryState(value: unknown): PackageSummary | undefined {
  if (value == null) return undefined;
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new ReadError("invalid");
  const raw = value as Record<string, unknown>, name = packageName(raw.name);
  const rawDesc = typeof raw.description === "string" ? raw.description : "";
  const rawLicense = typeof raw.license === "string" ? raw.license : "";
  if (/[\ud800-\udfff]/u.test(rawDesc) || /[\u0000-\u001f\u007f\ud800-\udfff]/u.test(rawLicense)) throw new ReadError("invalid");
  const summary: PackageSummary = { name, version: typeof raw.version === "string" && /^[0-9A-Za-z][0-9A-Za-z.+-]{0,99}$/.test(raw.version) ? raw.version : (() => { throw new ReadError("invalid"); })(), description: clip(rawDesc, 500), license: clip(rawLicense, 120), url: "" };
  const url = new URL(typeof raw.url === "string" ? raw.url : "");
  if (url.protocol !== "https:" || url.hostname !== "www.npmjs.com" || url.username || url.password || url.port || url.href.length > 4096 || url.pathname !== `/package/${name}`) throw new ReadError("invalid");
  summary.url = url.href;
  for (const field of ["repository", "homepage"] as const) {
    if (raw[field] == null) continue;
    if (typeof raw[field] !== "string") throw new ReadError("invalid");
    const link = new URL(raw[field]);
    if (link.protocol !== "https:" || link.username || link.password || link.port || link.href.length > 4096) throw new ReadError("invalid");
    summary[field] = link.href;
  }
  if (raw.published != null) {
    if (typeof raw.published !== "string" || !Number.isFinite(Date.parse(raw.published))) throw new ReadError("invalid");
    summary.published = raw.published;
  }
  for (const field of ["weeklyDownloads", "monthlyDownloads"] as const) {
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
    return { q: queryText(value.q), found: value.found, package: summaryState(value.package) };
  } catch { return initialState; }
}
export const encodeState = (state: PageState) => JSON.stringify(state);
function boundState(state: PageState): PageState {
  const base: PageState = { q: clip(state.q, 200), found: Boolean(state.found) };
  if (!state.package) return base;
  const candidate: PageState = { ...base, package: { ...state.package, description: clip(state.package.description, 500), license: clip(state.package.license, 120) } };
  return encoder.encode(encodeState(candidate)).byteLength <= MAX_STATE_BYTES ? candidate : base;
}
export function packageTransition(state: PageState, result: SearchResult, id: string): PageState {
  if (!id.startsWith("package:")) return state;
  const pkg = result.packages.find(candidate => id === `package:${candidate.name}` || id === packageActionID(candidate.name));
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
export async function loadPage(service: NpmPackages, state: PageState, refresh: boolean, signal?: AbortSignal): Promise<Loaded> {
  const request = budget(signal, service.limits.page);
  try {
    if (!state.found) return { kind: "empty" };
    if (state.package) return { kind: "detail", result: await service.package(state.package.name, refresh, request.signal) };
    return { kind: "results", result: await service.search(state.q, 12, refresh, request.signal) };
  } catch (error) {
    checkSignal(signal);
    return { kind: "error", error: !(error instanceof ReadError) && request.signal.aborted && request.signal.reason instanceof ReadError ? request.signal.reason : error };
  }
  finally { request.dispose(); }
}
export interface PageProps { service: NpmPackages; state: PageState; loaded: Loaded; locale?: string; uiVersion?: number; action?: (id: string, next?: PageState, title?: string) => NativeAction | CallbackAction }
export function page(props: PageProps): Node {
  const { service, state, loaded, uiVersion = 0 } = props, locale = props.locale || service.locale, t = textsFor(locale);
  const safeState = boundState(state);
  const encodedState = encodeState(safeState);
  const act = (id: string, next?: PageState, title?: string) => {
    const safeNext = next ? boundState(next) : undefined;
    const safeTitle = title ? clip(title, 2000) : undefined;
    return props.action?.(id, safeNext, safeTitle) ?? (safeNext && uiVersion >= 3 ? { id, push: { ui: "packages", state: encodeState(safeNext), ...(safeTitle ? { title: safeTitle } : {}) } } : { id });
  };
  const text = (key: string, value: string, style: "body" | "caption" | "secondary" = "secondary") => <Text key={key} text={clip(value || t.unknown, 2000)} style={style} />;
  const search = <Section key="npmpackages-search" title={t.search}><Field key="npmpackages-query" name="query" label={t.search} kind="text" placeholder={t.searchHint} value={safeState.q} /><Button key="npmpackages-submit" title={t.searchAction} action={act("search")} /></Section>;
  const attribution = <Section key="npmpackages-attribution">{text("npmpackages-license", t.attribution, "caption")}</Section>;
  const footer = <Section key="npmpackages-footer"><Button key="npmpackages-refresh" title={t.retry} action={act("refresh")} />{safeState.package ? <Button key="npmpackages-back" title={t.back} action={act("back")} /> : null}<Button key="npmpackages-home" title={t.home} action={act("home")} /></Section>;
  const stale = (error?: ReadError) => error ? <Section key="npmpackages-stale">{text("npmpackages-stale-text", errorText(error, locale))}</Section> : null;
  if (loaded.kind === "loading") return <View key="npmpackages" title={t.search} state={encodedState}>{search}<Section key="npmpackages-loading"><Loading key="npmpackages-loading-indicator" title={t.loading} /></Section>{attribution}</View>;
  if (loaded.kind === "error") return <View key="npmpackages" title={t.search} state={encodedState}>{search}<Section key="npmpackages-error">{text("npmpackages-error-message", errorText(loaded.error, locale))}<Button key="npmpackages-retry" title={t.retry} action={act("refresh")} /></Section>{attribution}</View>;
  if (loaded.kind === "empty") return <View key="npmpackages" title={t.search} state={encodedState}>{search}<Section key="npmpackages-welcome">{text("npmpackages-welcome-text", t.welcome, "body")}</Section>{attribution}</View>;
  if (loaded.kind === "detail") {
    const { package: pkg, downloadsStart, downloadsEnd } = loaded.result;
    const pkgURL = validHTTPS(pkg.url) ?? `https://www.npmjs.com/package/${pkg.name}`;
    const repoURL = validHTTPS(pkg.repository);
    const homeURL = validHTTPS(pkg.homepage);
    const publishedText = pkg.published ? dateText(pkg.published) : "";
    const detailFooter = clip([fmt(t.license, { license: pkg.license }), publishedText ? fmt(t.published, { version: pkg.version, date: publishedText }) : `v${pkg.version}`].join(" · "), 2000);
    const links = <Section key="npmpackages-links"><Link key="npmpackages-npm-link" title={t.openPackage} url={pkgURL} />{repoURL ? <Link key="npmpackages-repository-link" title={t.openRepository} url={repoURL} /> : null}{homeURL ? <Link key="npmpackages-homepage-link" title={t.openHomepage} url={homeURL} /> : null}</Section>;
    return <View key="npmpackages" title={clip(pkg.name, 2000)} state={encodedState}>{search}
      <Section key="npmpackages-detail" title={clip(pkg.name, 2000)} footer={detailFooter}>
        {text("npmpackages-description", pkg.description, "body")}
        {text("npmpackages-downloads", pkg.weeklyDownloads === undefined ? t.noDownloads : `${fmt(t.weekly, { count: number(pkg.weeklyDownloads) })}${downloadsStart && downloadsEnd ? ` · ${downloadsStart} – ${downloadsEnd}` : ""}`)}
        {text("npmpackages-dependencies", fmt(t.dependencies, { count: pkg.dependencies }))}
        {text("npmpackages-engines", `${t.engines}: ${pkg.engines}`)}
        {pkg.unpackedSize !== undefined ? text("npmpackages-size", fmt(t.size, { count: bytes(pkg.unpackedSize) })) : null}
        {text("npmpackages-types", pkg.types ? t.types : t.typesNone, "caption")}
      </Section>
      <Section key="npmpackages-install" title={t.install}>{text("npmpackages-install-command", pkg.install, "body")}{text("npmpackages-install-hint", t.installHint, "caption")}</Section>
      {links}{stale(loaded.result.stale)}{footer}{attribution}
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
  for (const pkg of loaded.result.packages) {
    if (remainingItems < 3) break;
    const title = clip(`${pkg.name}@${pkg.version}`, 2000);
    const publishedText = pkg.published ? dateText(pkg.published) : "";
    const cardFooter = clip([fmt(t.license, { license: pkg.license }), pkg.weeklyDownloads !== undefined ? fmt(t.weekly, { count: number(pkg.weeklyDownloads) }) : "", publishedText].filter(Boolean).join(" · "), 2000);
    const desc = clip(pkg.description || t.unknown, 2000);
    const pkgURL = validHTTPS(pkg.url) ?? `https://www.npmjs.com/package/${pkg.name}`;
    const sectionKey = uniqueID("package-", pkg.name);
    const descKey = uniqueID("description-", pkg.name);
    const openKey = uniqueID("open-", pkg.name);
    const linkKey = uniqueID("link-", pkg.name);
    const nextState = boundState({ ...safeState, package: pkg });
    const actionID = packageActionID(pkg.name);
    const cardBytes = encoder.encode(JSON.stringify([sectionKey, title, cardFooter, descKey, desc, openKey, actionID, linkKey, pkgURL, nextState])).byteLength;
    if (cardBytes > remainingBytes) break;
    cards.push(<Section key={sectionKey} title={title} footer={cardFooter}>
      {text(descKey, desc, "body")}
      <Button key={openKey} title={t.details} action={act(actionID, nextState, pkg.name)} />
      <Link key={linkKey} title={t.openPackage} url={pkgURL} />
    </Section>);
    remainingItems -= 3;
    remainingBytes -= cardBytes;
  }
  return <View key="npmpackages" title={clip(`${t.title}: ${clip(safeState.q, 100)}`, 2000)} state={encodedState}>{search}
    <Section key="npmpackages-result-count">{text("npmpackages-total", fmt(t.results, { total: number(loaded.result.total) }), "caption")}</Section>
    {cards.length ? cards : <Section key="npmpackages-empty-results">{text("npmpackages-empty", t.empty)}</Section>}
    {stale(loaded.result.stale)}{footer}{attribution}
  </View>;
}
