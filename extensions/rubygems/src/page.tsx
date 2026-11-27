import { Button, Field, Link, Loading, Section, Text, View, type CallbackAction, type NativeAction, type Node } from "@persona/extension/ui";
import { boundedID, clip, gemName, httpsURL, queryText, ReadError, type DetailResult, type GemSummary, type SearchResult } from "./model.ts";
import { dateText, errorText, fmt, number, textsFor } from "./i18n.ts";
import { budget, checkSignal, type RubyGems } from "./service.ts";
const TEXT_MAX = 2000;
const KEY_MAX = 128;
const ITEMS_MAX = 300;
const STATE_MAX = 16_384;
const FRAME_MAX = 512 * 1024;
const utf8 = new TextEncoder();
const byteLength = (text: string) => utf8.encode(text).byteLength;
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
export interface PageState { q: string; found: boolean; gem?: GemSummary }
export type Loaded = { kind: "loading" } | { kind: "empty" } | { kind: "results"; result: SearchResult } | { kind: "detail"; result: DetailResult } | { kind: "error"; error: unknown };
export const initialState: PageState = { q: "", found: false };
function summaryState(value: unknown): GemSummary | undefined {
  if (value == null) return undefined;
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new ReadError("invalid");
  const raw = value as Record<string, unknown>, name = gemName(raw.name);
  if (typeof raw.version !== "string" || !/^[0-9A-Za-z][0-9A-Za-z.+-]{0,99}$/.test(raw.version) || typeof raw.totalDownloads !== "number" || !Number.isSafeInteger(raw.totalDownloads) || raw.totalDownloads < 0 || raw.totalDownloads > 2_000_000_000_000) throw new ReadError("invalid");
  if (raw.description != null && (typeof raw.description !== "string" || /[\ud800-\udfff]/u.test(raw.description))) throw new ReadError("invalid");
  if (raw.license != null && (typeof raw.license !== "string" || /[\u0000-\u001f\u007f\ud800-\udfff]/u.test(raw.license))) throw new ReadError("invalid");
  const summary: GemSummary = { name, version: raw.version, description: clip(typeof raw.description === "string" ? raw.description : "", 500), license: clip(typeof raw.license === "string" ? raw.license : "", 120), url: `https://rubygems.org/gems/${name}`, totalDownloads: raw.totalDownloads };
  for (const field of ["repository", "homepage", "documentation"] as const) {
    if (raw[field] == null) continue;
    const link = httpsURL(raw[field]);
    if (!link) throw new ReadError("invalid");
    summary[field] = link;
  }
  if (raw.versionDownloads != null) {
    if (typeof raw.versionDownloads !== "number" || !Number.isSafeInteger(raw.versionDownloads) || raw.versionDownloads < 0 || raw.versionDownloads > 2_000_000_000_000) throw new ReadError("invalid");
    summary.versionDownloads = raw.versionDownloads;
  }
  return summary;
}
export function readState(raw: unknown): PageState {
  if (typeof raw !== "string" || byteLength(raw) > STATE_MAX) return initialState;
  try {
    const value = JSON.parse(raw) as Record<string, unknown>;
    if (!value || Array.isArray(value) || typeof value.found !== "boolean") return initialState;
    if (!value.found && (value.q === "" || value.q == null) && value.gem == null) return initialState;
    return { q: queryText(value.q), found: value.found, ...(value.gem == null ? {} : { gem: summaryState(value.gem) }) };
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
export function gemTransition(state: PageState, result: SearchResult, id: string): PageState {
  if (!id.startsWith("gem:")) return state;
  const gem = result.gems.find(candidate => id === `gem:${candidate.name}`);
  return gem ? { q: state.q, found: true, gem } : state;
}
export function transition(state: PageState, id: string, values: unknown = {}): PageState {
  let fields: Readonly<Record<string, unknown>>;
  if (typeof values === "string") { try { values = JSON.parse(values); } catch { return state; } }
  if (!values || typeof values !== "object" || Array.isArray(values)) return state;
  fields = values as Readonly<Record<string, unknown>>;
  if (id === "search") {
    try {
      const next = { q: queryText(fields.query), found: true };
      return next.q === state.q && state.found ? { ...next, gem: state.gem } : next;
    } catch { return state; }
  }
  if (id === "home") return initialState;
  if (id === "back" && state.found) return { q: state.q, found: true };
  return state;
}
export async function loadPage(service: RubyGems, state: PageState, refresh: boolean, signal?: AbortSignal): Promise<Loaded> {
  const request = budget(signal, service.limits.page);
  try {
    if (!state.found) return { kind: "empty" };
    if (state.gem) return { kind: "detail", result: await service.gem(state.gem.name, refresh, request.signal) };
    return { kind: "results", result: await service.search(state.q, 12, refresh, request.signal) };
  } catch (error) { checkSignal(signal); return { kind: "error", error }; }
  finally { request.dispose(); }
}
export interface PageProps { service: RubyGems; state: PageState; loaded: Loaded; locale?: string; uiVersion?: number; action?: (id: string, next?: PageState, title?: string) => NativeAction | CallbackAction }
export function page(props: PageProps): Node {
  const { service, loaded, uiVersion = 0 } = props, state = safeState(props.state), stateJSON = encodeState(state), t = textsFor(props.locale ?? service.locale);
  const act = (id: string, next?: PageState, title?: string) => {
    const cleanTitle = title ? clip(title, TEXT_MAX) : undefined;
    return props.action?.(id, next, cleanTitle) ?? (next && uiVersion >= 3 ? { id, push: { ui: "rubygems", state: encodeState(next), ...(cleanTitle ? { title: cleanTitle } : {}) } } : { id });
  };
  const text = (key: string, value: string, style: "body" | "caption" | "secondary" = "secondary") => <Text key={key} text={clip(value || t.unknown, TEXT_MAX)} style={style} />;
  const search = <Section key="rubygems-search" title={clip(t.search, TEXT_MAX)}><Field key="rubygems-query" name="query" label={clip(t.search, TEXT_MAX)} kind="text" placeholder={clip(t.searchHint, TEXT_MAX)} value={state.q} /><Button key="rubygems-submit" title={clip(t.searchAction, TEXT_MAX)} action={act("search")} /></Section>;
  const attribution = <Section key="rubygems-attribution">{text("rubygems-license", t.attribution, "caption")}</Section>;
  const footer = <Section key="rubygems-footer"><Button key="rubygems-refresh" title={clip(t.retry, TEXT_MAX)} action={act("refresh")} />{state.gem ? <Button key="rubygems-back" title={clip(t.back, TEXT_MAX)} action={act("back")} /> : null}<Button key="rubygems-home" title={clip(t.home, TEXT_MAX)} action={act("home")} /></Section>;
  const stale = (error?: ReadError) => error ? <Section key="rubygems-stale">{text("rubygems-stale-text", errorText(error, props.locale ?? service.locale))}</Section> : null;
  if (loaded.kind === "loading") return <View key="rubygems" title={clip(t.search, TEXT_MAX)} state={stateJSON}>{search}<Section key="rubygems-loading"><Loading key="rubygems-loading-indicator" title={clip(t.loading, TEXT_MAX)} /></Section>{attribution}</View>;
  if (loaded.kind === "error") return <View key="rubygems" title={clip(t.search, TEXT_MAX)} state={stateJSON}>{search}<Section key="rubygems-error">{text("rubygems-error-message", errorText(loaded.error, props.locale ?? service.locale))}<Button key="rubygems-retry" title={clip(t.retry, TEXT_MAX)} action={act("refresh")} /></Section>{attribution}</View>;
  if (loaded.kind === "empty") return <View key="rubygems" title={clip(t.search, TEXT_MAX)} state={stateJSON}>{search}<Section key="rubygems-welcome">{text("rubygems-welcome-text", t.welcome, "body")}</Section>{attribution}</View>;
  if (loaded.kind === "detail") {
    const gem = loaded.result.gem;
    const usedDetailKeys = new Set(["rubygems-search", "rubygems-query", "rubygems-submit", "rubygems-detail", "rubygems-description", "rubygems-total", "rubygems-version-downloads", "rubygems-yanked", "rubygems-dependencies", "rubygems-no-dependencies", "rubygems-install", "rubygems-install-command", "rubygems-install-hint", "rubygems-links", "rubygems-gem-link", "rubygems-docs-link", "rubygems-repository-link", "rubygems-homepage-link", "rubygems-stale", "rubygems-stale-text", "rubygems-footer", "rubygems-refresh", "rubygems-back", "rubygems-home", "rubygems-attribution", "rubygems-license"]);
    const gemURL = httpsURL(gem.url), docsURL = httpsURL(gem.documentation), repoURL = httpsURL(gem.repository), homeURL = httpsURL(gem.homepage);
    const hasLinks = Boolean(gemURL || docsURL || repoURL || homeURL);
    return <View key="rubygems" title={clip(gem.name, TEXT_MAX)} state={stateJSON}>{search}
      <Section key="rubygems-detail" title={clip(`${gem.name}@${gem.version}`, TEXT_MAX)} footer={clip([fmt(t.license, { license: gem.license }), gem.published ? dateText(gem.published) : ""].filter(Boolean).join(" · "), TEXT_MAX)}>
        {text("rubygems-description", gem.description, "body")}
        {text("rubygems-total", fmt(t.total, { count: number(gem.totalDownloads) }))}
        {gem.versionDownloads !== undefined ? text("rubygems-version-downloads", fmt(t.versionDownloads, { count: number(gem.versionDownloads) })) : null}
        {gem.yanked ? text("rubygems-yanked", t.yanked, "caption") : null}
      </Section>
      <Section key="rubygems-dependencies" title={clip(fmt(t.dependencies, { count: number(gem.dependencies.length) }), TEXT_MAX)}>
        {gem.dependencies.length ? gem.dependencies.slice(0, 30).map(dependency => text(uniqueKey(usedDetailKeys, "dependency-", dependency.name), `${dependency.name} (${dependency.requirements})`)) : text("rubygems-no-dependencies", t.noDependencies)}
      </Section>
      <Section key="rubygems-install" title={clip(t.install, TEXT_MAX)}>{text("rubygems-install-command", gem.install, "body")}{text("rubygems-install-hint", t.installHint, "caption")}</Section>
      {hasLinks ? <Section key="rubygems-links">{gemURL ? <Link key="rubygems-gem-link" title={clip(t.openGem, TEXT_MAX)} url={gemURL} /> : null}{docsURL ? <Link key="rubygems-docs-link" title={clip(t.openDocs, TEXT_MAX)} url={docsURL} /> : null}{repoURL ? <Link key="rubygems-repository-link" title={clip(t.openRepository, TEXT_MAX)} url={repoURL} /> : null}{homeURL ? <Link key="rubygems-homepage-link" title={clip(t.openHomepage, TEXT_MAX)} url={homeURL} /> : null}</Section> : null}
      {stale(loaded.result.stale)}{footer}{attribution}
    </View>;
  }
  const usedKeys = new Set(["rubygems-search", "rubygems-query", "rubygems-submit", "rubygems-result-count", "rubygems-total-results", "rubygems-empty-results", "rubygems-empty", "rubygems-stale", "rubygems-stale-text", "rubygems-footer", "rubygems-refresh", "rubygems-back", "rubygems-home", "rubygems-attribution", "rubygems-license"]);
  const reservedItems = 2 + 1 + (loaded.result.stale ? 1 : 0) + (state.gem ? 3 : 2) + 1;
  let remainingItems = Math.max(0, ITEMS_MAX - reservedItems);
  let remainingBytes = FRAME_MAX - byteLength(stateJSON) - 8192;
  const cards: Node[] = [];
  for (const gem of loaded.result.gems) {
    const gemURL = httpsURL(gem.url);
    const neededItems = gemURL ? 3 : 2;
    if (neededItems > remainingItems) break;
    const sectionKey = uniqueKey(usedKeys, "gem-", gem.name);
    const descKey = uniqueKey(usedKeys, "description-", gem.name);
    const openKey = uniqueKey(usedKeys, "open-", gem.name);
    const linkKey = gemURL ? uniqueKey(usedKeys, "link-", gem.name) : "";
    const title = clip(`${gem.name}@${gem.version}`, TEXT_MAX);
    const cardFooter = clip([fmt(t.license, { license: gem.license }), fmt(t.total, { count: number(gem.totalDownloads) })].join(" · "), TEXT_MAX);
    const description = clip(gem.description || t.unknown, TEXT_MAX);
    const nextState: PageState = { ...state, gem };
    const nextStateJSON = uiVersion >= 3 ? encodeState(nextState) : "";
    const actionID = `gem:${gem.name}`;
    const estimatedBytes = byteLength(sectionKey) + byteLength(descKey) + byteLength(openKey) + byteLength(linkKey) + byteLength(title) + byteLength(cardFooter) + byteLength(description) + byteLength(nextStateJSON) + byteLength(actionID) + (gemURL ? byteLength(gemURL) : 0) + 512;
    if (estimatedBytes > remainingBytes) break;
    remainingItems -= neededItems;
    remainingBytes -= estimatedBytes;
    cards.push(<Section key={sectionKey} title={title} footer={cardFooter}>
      {text(descKey, description, "body")}
      <Button key={openKey} title={clip(t.details, TEXT_MAX)} action={act(actionID, nextState, gem.name)} />
      {gemURL ? <Link key={linkKey} title={clip(t.openGem, TEXT_MAX)} url={gemURL} /> : null}
    </Section>);
  }
  return <View key="rubygems" title={clip(`${t.title}: ${clip(state.q, 100)}`, TEXT_MAX)} state={stateJSON}>{search}
    <Section key="rubygems-result-count">{text("rubygems-total-results", fmt(t.results, { total: number(loaded.result.gems.length) }), "caption")}</Section>
    {cards.length ? cards : <Section key="rubygems-empty-results">{text("rubygems-empty", t.empty)}</Section>}
    {stale(loaded.result.stale)}{footer}{attribution}
  </View>;
}
