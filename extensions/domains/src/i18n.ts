import en from "./locales/en.json";
import zh from "./locales/zh-Hans.json";
import ja from "./locales/ja.json";
import { ReadError } from "./model.ts";
export type Texts = typeof en;
export const textsFor = (locale = ""): Texts => locale.toLowerCase().startsWith("ja") ? ja : locale.toLowerCase().startsWith("zh") ? zh : en;
export const fmt = (text: string, values: Record<string, string | number>) => text.replace(/\{(\w+)\}/g, (_, key) => String(values[key] ?? ""));
export function errorText(error: unknown, locale = ""): string {
  const t = textsFor(locale);
  if (!(error instanceof ReadError)) return t.network;
  return error.code === "http" ? fmt(t.http, { 0: error.status ?? 0 }) : t[error.code];
}
export function readTime(at: number | string, timeZone = "Local", locale = "en"): string {
  const value = new Date(at);
  if (Number.isNaN(value.getTime())) return "";
  const tz = timeZone === "Local" ? undefined : timeZone;
  try { return new Intl.DateTimeFormat(locale, { year: "numeric", month: "short", day: "numeric", timeZone: tz }).format(value); } catch {}
  try { return new Intl.DateTimeFormat(locale, { year: "numeric", month: "short", day: "numeric", timeZone: "UTC" }).format(value); } catch {}
  try { return new Intl.DateTimeFormat("en", { year: "numeric", month: "short", day: "numeric", timeZone: "UTC" }).format(value); } catch {}
  return value.toISOString().slice(0, 10);
}
