import { Button, Field, Link, Loading, Row, Section, Text, View, type CallbackAction, type NativeAction, type Node } from "@persona/extension/ui";
import { clip, domainName, ReadError, statusURL, type Registration } from "./model.ts";
import { errorText, fmt, readTime, textsFor } from "./i18n.ts";
import { budget, checkSignal, type Domains } from "./service.ts";
import type { HistoryStorage } from "./store.ts";
export type ViewKind = "lookup" | "result" | "history";
export interface PageState { v: ViewKind; q: string; message?: string }
export type Loaded = { kind: "loading" } | { kind: "empty" } | { kind: "result"; result: Registration } | { kind: "history"; domains: string[] } | { kind: "error"; error: unknown };
export const initialState: PageState = { v: "lookup", q: "" };
const STATE_MAX = 16_384;
const ITEMS_MAX = 300;
const utf8Bytes = (value: string) => new TextEncoder().encode(value).byteLength;
function hash64(value: string): string {
  let hash = 0xcbf29ce484222325n;
  const mask = (1n << 64n) - 1n;
  for (const byte of new TextEncoder().encode(value)) {
    hash ^= BigInt(byte);
    hash = (hash * 0x100000001b3n) & mask;
  }
  return hash.toString(16).padStart(16, "0");
}
function boundedID(prefix: string, value: string, max = 128): string {
  const full = `${prefix}${value}`;
  if (full.length <= max) return full;
  const suffix = `~${hash64(value)}`;
  const keep = Math.max(0, max - prefix.length - suffix.length);
  return `${prefix}${value.slice(0, keep)}${suffix}`;
}
export const domainActionID = (domain: string): string => boundedID("domain:", domain, 128);
function uniqueKey(used: Set<string>, prefix: string, value: string): string {
  let candidate = boundedID(prefix, value, 128);
  let attempt = 1;
  while (used.has(candidate)) {
    candidate = boundedID(prefix, `${value}#${attempt++}`, 128);
  }
  used.add(candidate);
  return candidate;
}
function safeLinkURL(raw: string): string | undefined {
  try {
    const url = new URL(raw);
    if (url.protocol !== "https:" || url.username || url.password || url.port || url.hostname === "localhost" || url.hostname === "127.0.0.1" || url.hostname === "::1" || !url.hostname.includes(".")) return undefined;
    url.hash = "";
    const href = url.toString();
    return href.length <= 4096 ? href : undefined;
  } catch { return undefined; }
}
function safeState(state: PageState): PageState {
  if (!state || typeof state !== "object") return initialState;
  const v: ViewKind = state.v === "result" || state.v === "history" ? state.v : "lookup";
  const q = typeof state.q === "string" ? clip(state.q.trim(), 253) : "";
  const message = state.message === "cleared" ? "cleared" : undefined;
  if (v === "result") {
    try { return { v: "result", q: domainName(q).ascii }; }
    catch { return initialState; }
  }
  return { v, q, ...(message ? { message } : {}) };
}
export function readState(raw: unknown): PageState {
  if (typeof raw !== "string" || raw.length > 2000) return initialState;
  try {
    const value = JSON.parse(raw) as Record<string, unknown>;
    if (!value || Array.isArray(value)) return initialState;
    const q = typeof value.q === "string" ? clip(value.q.trim(), 253) : "";
    const v = ["result", "history"].includes(String(value.v)) ? value.v as ViewKind : "lookup";
    if (v === "result") return { v, q: domainName(q).ascii, ...(value.message === "cleared" ? { message: "cleared" } : {}) };
    return { v, q, ...(value.message === "cleared" ? { message: "cleared" } : {}) };
  } catch { return initialState; }
}
export function encodeState(state: PageState): string {
  const clean = safeState(state);
  const encoded = JSON.stringify(clean);
  if (utf8Bytes(encoded) <= STATE_MAX) return encoded;
  return JSON.stringify(initialState);
}
export function transition(state: PageState, id: string, values: unknown = {}): PageState {
  if (id === "lookup") return { v: "lookup", q: "" };
  if (id === "history") return { v: "history", q: "" };
  if (id === "cleared") return { v: "history", q: "", message: "cleared" };
  if (id === "back") return { v: "lookup", q: state.q };
  if (id.startsWith("domain:")) {
    try { return { v: "result", q: domainName(id.slice(7)).ascii }; } catch { return state; }
  }
  if (id === "search") {
    let fields: unknown = values;
    if (typeof values === "string") { try { fields = JSON.parse(values); } catch { return state; } }
    if (!fields || typeof fields !== "object" || Array.isArray(fields) || typeof (fields as { domain?: unknown }).domain !== "string") return state;
    try { return { v: "result", q: domainName((fields as { domain: string }).domain).ascii }; } catch { return state; }
  }
  return state;
}
export async function loadPage(service: Domains, history: HistoryStorage, state: PageState, refresh: boolean, signal?: AbortSignal): Promise<Loaded> {
  const request = budget(signal, service.limits.page);
  try {
    if (state.v === "result") {
      const result = await service.lookup(state.q, refresh, request.signal);
      try { await history.add(result.domain.ascii); } catch { /* Read-only results do not depend on history. */ }
      return { kind: "result", result };
    }
    if (state.v === "history") return { kind: "history", domains: (await history.load()).domains };
    return { kind: "empty" };
  } catch (error) {
    checkSignal(signal);
    return { kind: "error", error: request.signal.aborted && request.signal.reason instanceof ReadError ? request.signal.reason : error };
  }
  finally { request.dispose(); }
}
export interface PageProps { service: Domains; history: HistoryStorage; state: PageState; loaded: Loaded; locale?: string; tz?: string; uiVersion?: number; action?: (id: string, next?: PageState, title?: string) => NativeAction | CallbackAction }
export function page(props: PageProps): Node {
  const { service, loaded } = props, state = safeState(props.state), locale = props.locale ?? service.locale, t = textsFor(locale), tz = props.tz ?? "Local";
  const act = (id: string, next?: PageState, title?: string) => props.action?.(id, next, title) ?? (next && (props.uiVersion ?? 0) >= 3 ? { id, push: { ui: "domains", state: encodeState(next), ...(title ? { title: clip(title, 200) } : {}) } } : { id });
  const text = (key: string, value: string, style: "body" | "caption" | "secondary" = "secondary") => <Text key={key} text={clip(value, 1800)} style={style} />;
  const form = <Section key="domain-form" title={clip(t.search, 200)}><Field key="domain-input" name="domain" label={clip(t.search, 200)} kind="text" placeholder={clip(t.hint, 200)} value={state.q} /><Button key="domain-submit" title={clip(t.action, 200)} action={act("search")} /></Section>;
  const nav = <Section key="domain-navigation"><Button key="domain-home" title={clip(t.lookup, 200)} action={act("lookup")} /><Button key="domain-history" title={clip(t.history, 200)} action={act("history")} /></Section>;
  if (loaded.kind === "loading") return <View key="domains" title={clip(t.lookup, 200)} state={encodeState(state)}>{form}{nav}<Section key="domain-loading"><Loading key="domain-loading-indicator" title={clip(t.loading, 200)} /></Section></View>;
  if (loaded.kind === "error") return <View key="domains" title={clip(t.lookup, 200)} state={encodeState(state)}>{form}{nav}<Section key="domain-error">{text("domain-error-message", errorText(loaded.error, locale))}<Button key="domain-retry" title={clip(t.retry, 200)} action={act("refresh")} /></Section></View>;
  if (state.v === "result") {
    if (loaded.kind !== "result") throw new ReadError("invalid");
    const result = loaded.result;
    const used = new Set<string>();
    const safeRecordURL = safeLinkURL(result.url);
    const safeEvents = result.events.slice(0, Math.min(32, ITEMS_MAX - 20));
    const readLabel = readTime(result.at, tz, locale);
    const footerText = clip(`${fmt(t.readAt, { at: readLabel })}${result.stale ? ` · ${errorText(result.stale, locale)}` : ""}`, 1000);
    return <View key="domains" title={clip(result.domain.unicode, 200)} state={encodeState(state)}>{form}{nav}
      <Section key="registration" title={clip(fmt(t.result, { domain: result.domain.unicode }), 200)}>
        {text("registered-name", `${t.registeredName}: ${result.name}`, "body")}
        {result.registrar ? text("registrar", `${t.registrar}: ${result.registrar}`, "body") : null}
      </Section>
      <Section key="registration-status" title={clip(t.statuses, 200)}>{text("registration-status-values", result.statuses.join(" · ") || t.noStatus, "body")}<Link key="status-glossary" title={clip(t.statusGlossary, 200)} url={statusURL} /></Section>
      <Section key="registration-events" title={clip(t.events, 200)}>{safeEvents.length ? safeEvents.map((event, index) => <Row key={uniqueKey(used, "event-", String(index))} title={clip(event.action, 200)} subtitle={clip(readTime(event.at, tz, locale), 300)} />) : text("no-events", t.noEvents)}</Section>
      <Section key="registration-source" title={clip(t.source, 200)}>{text("source-name", result.source, "body")}{safeRecordURL ? <Link key="registration-open" title={clip(t.openRecord, 200)} url={safeRecordURL} /> : null}<Button key="registration-back" title={clip(t.lookup, 200)} action={act("back")} /></Section>
      <Section key="registration-footer" footer={footerText}><Button key="registration-refresh" title={clip(t.refresh, 200)} action={act("refresh")} /></Section>
      <Section key="registration-notes">{text("status-note", t.statusHint, "caption")}{text("registration-attribution", t.attribution, "caption")}</Section>
    </View>;
  }
  if (state.v === "history") {
    if (loaded.kind !== "history") throw new ReadError("invalid");
    const used = new Set<string>();
    const domains = loaded.domains.slice(0, Math.min(20, ITEMS_MAX - 10));
    return <View key="domains" title={clip(t.history, 200)} state={encodeState(state)}>{nav}
      {state.message === "cleared" ? <Section key="history-cleared">{text("history-cleared-text", t.cleared, "body")}</Section> : null}
      <Section key="history-list" title={clip(t.history, 200)}>{domains.length ? domains.map(domain => <Row key={uniqueKey(used, "history-", domain)} title={clip(domain, 200)} action={act(domainActionID(domain), { v: "result", q: domain }, clip(domain, 200))} />) : text("history-empty", t.historyEmpty)}{domains.length ? <Button key="history-clear" title={clip(t.clearHistory, 200)} action={act("clear")} /> : null}</Section>
      <Section key="history-note">{text("history-note-text", t.historyHint, "caption")}</Section>
    </View>;
  }
  return <View key="domains" title={clip(t.lookup, 200)} state={encodeState(state)}>{form}{nav}<Section key="domain-welcome">{text("domain-welcome-text", t.attribution, "body")}</Section></View>;
}
