import { Button, Field, Link, Loading, Section, Text, View, type CallbackAction, type NativeAction, type Node } from "@persona/extension/ui";
import { clip, ReadError, word, type Entry } from "./model.ts";
import { errorText, fmt, readTime, textsFor } from "./i18n.ts";
import { budget, checkSignal, type Wiktionary } from "./service.ts";
export interface PageState { q: string; found: boolean }
export type Loaded = { kind: "loading" } | { kind: "empty" } | { kind: "entry"; entry: Entry } | { kind: "error"; error: unknown };
export const initialState: PageState = { q: "", found: false };
const MAX_VIEW_ITEMS = 300;
const ENTRY_CHROME_ITEMS = 6;
const MAX_SENSE_ITEMS = MAX_VIEW_ITEMS - ENTRY_CHROME_ITEMS;
const MAX_SENSE_BYTES = 512 * 1024;
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
  const suffix = "-" + shortHash(value);
  const keep = max - Array.from(prefix).length - suffix.length;
  return prefix + Array.from(value).slice(0, Math.max(0, keep)).join("") + suffix;
}
export function readState(raw: unknown): PageState {
  if (typeof raw !== "string" || raw.length > 1000) return initialState;
  try {
    const value = JSON.parse(raw) as Record<string, unknown>;
    if (!value || Array.isArray(value) || typeof value.q !== "string" || typeof value.found !== "boolean") return initialState;
    if (!value.q.trim()) return initialState;
    return { q: word(value.q), found: value.found };
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
  if (id === "lookup") {
    const query = typeof fields.word === "string" ? fields.word.trim() : "";
    try { return query ? { q: word(query), found: true } : state; } catch { return state; }
  }
  if (id === "home") return initialState;
  return state;
}
export async function loadPage(service: Wiktionary, state: PageState, refresh: boolean, signal?: AbortSignal): Promise<Loaded> {
  const request = budget(signal, service.limits.page);
  try {
    if (!state.found) return { kind: "empty" };
    return { kind: "entry", entry: await service.lookup(state.q, refresh, request.signal) };
  } catch (error) {
    checkSignal(signal);
    return { kind: "error", error: !(error instanceof ReadError) && request.signal.aborted && request.signal.reason instanceof ReadError ? request.signal.reason : error };
  }
  finally { request.dispose(); }
}
export interface PageProps { service: Wiktionary; state: PageState; loaded: Loaded; locale?: string; tz?: string; uiVersion?: number; action?: (id: string, next?: PageState, title?: string) => NativeAction | CallbackAction }
export function page(props: PageProps): Node {
  const { service, state, loaded, uiVersion = 0 } = props, locale = props.locale || service.locale, t = textsFor(locale), tz = props.tz || "Local";
  const act = (id: string, next?: PageState, title?: string) => props.action?.(id, next, title) ?? (next && uiVersion >= 3 ? { id, push: { ui: "wiktionary", state: encodeState(next), ...(title ? { title } : {}) } } : { id });
  const text = (key: string, value: string, style: "body" | "caption" | "secondary" = "secondary") => <Text key={key} text={clip(value, 2000)} style={style} />;
  const search = <Section key="dictionary-search" title={t.lookup}><Field key="dictionary-word" name="word" label={t.lookup} kind="text" placeholder={t.searchHint} value={state.q} /><Button key="dictionary-submit" title={t.searchAction} action={act("lookup")} /></Section>;
  const home = <Section key="dictionary-home"><Button key="dictionary-home-button" title={t.back} action={act("home")} /></Section>;
  if (loaded.kind === "loading") return <View key="wiktionary" title={t.lookup} state={encodeState(state)}>{search}{state.found ? home : null}<Section key="dictionary-loading"><Loading key="dictionary-loading-indicator" title={t.loading} /></Section></View>;
  if (loaded.kind === "error") return <View key="wiktionary" title={t.lookup} state={encodeState(state)}>{search}{state.found ? home : null}<Section key="dictionary-error">{text("dictionary-error-message", errorText(loaded.error, locale))}<Button key="dictionary-retry" title={t.retry} action={act("refresh")} /></Section></View>;
  if (loaded.kind === "empty") return <View key="wiktionary" title={t.lookup} state={encodeState(state)}>{search}<Section key="dictionary-welcome">{text("dictionary-welcome-text", t.welcome, "body")}</Section><Section key="dictionary-attribution">{text("dictionary-license", t.attribution, "caption")}</Section></View>;
  const entry = loaded.entry;
  const sections: Node[] = [];
  let remainingItems = MAX_SENSE_ITEMS;
  let remainingBytes = MAX_SENSE_BYTES;
  for (let index = 0; index < entry.groups.length && remainingItems > 0; index++) {
    const group = entry.groups[index];
    const title = clip([group.partOfSpeech, group.language].filter(Boolean).join(" · "), 2000);
    const titleBytes = encoder.encode(JSON.stringify(title)).byteLength;
    if (titleBytes > remainingBytes) break;
    const items: Node[] = [];
    let sectionBytes = titleBytes;
    for (let i = 0; i < group.definitions.length && remainingItems > 0; i++) {
      const definition = group.definitions[i];
      const defText = clip(`${i + 1}. ${definition.text}`, 2000);
      const defBytes = encoder.encode(JSON.stringify(defText)).byteLength;
      if (sectionBytes + defBytes > remainingBytes) { remainingItems = 0; break; }
      items.push(text(`definition-${index}-${i}`, defText, "body"));
      remainingItems--;
      sectionBytes += defBytes;
      for (let j = 0; j < definition.examples.length && remainingItems > 0; j++) {
        const exampleText = clip(`${t.examples}: ${definition.examples[j].text}`, 2000);
        const exampleBytes = encoder.encode(JSON.stringify(exampleText)).byteLength;
        if (sectionBytes + exampleBytes > remainingBytes) { remainingItems = 0; break; }
        items.push(text(`example-${index}-${i}-${j}`, exampleText, "caption"));
        remainingItems--;
        sectionBytes += exampleBytes;
      }
    }
    if (items.length > 0) {
      remainingBytes -= sectionBytes;
      sections.push(<Section key={boundedID(`sense-${index}-`, group.partOfSpeech)} title={title}>{items}</Section>);
    }
  }
  const footer = clip(`${fmt(t.readAt, { at: readTime(entry.at, tz, locale) })}${entry.stale ? ` · ${errorText(entry.stale, locale)}` : ""}`, 2000);
  return <View key="wiktionary" title={clip(entry.word, 2000)} state={encodeState(state)}>{search}
    {sections}
    <Section key="dictionary-actions"><Link key="dictionary-open" title={t.open} url={entry.url} /><Button key="dictionary-back-button" title={t.back} action={act("home")} /></Section>
    <Section key="dictionary-footer" footer={footer}><Button key="dictionary-refresh" title={t.refresh} action={act("refresh")} /></Section>
    <Section key="dictionary-attribution">{text("dictionary-license", t.attribution, "caption")}</Section>
  </View>;
}
