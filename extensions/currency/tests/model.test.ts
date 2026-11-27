import { test } from "node:test";
import assert from "node:assert/strict";
import { amount, availableUnits, clip, currency, currencyConversion, formatForDisplay, multiply, parseRate, ratesPath, ReadError, roundedNumber, unit, unitConversion } from "../src/model.ts";
import { CurrencyService } from "../src/service.ts";
import { encodeState, initialState, page, readState, transition } from "../src/page.tsx";
import { Renderer } from "@persona/extension/ui";

test("ECB rate parsing requires the requested pair, a real date and a bounded positive rate", () => {
  const rate = parseRate([{ date: "2026-11-10", base: "USD", quote: "CNY", rate: 6.6703 }], "USD", "CNY", 1000);
  assert.deepEqual(rate, { base: "USD", quote: "CNY", rate: "6.6703", date: "2026-11-10", at: 1000 });
  assert.equal(currencyConversion("12.5", rate).result, "83.37875");
  assert.equal(ratesPath("USD", "CNY"), "/v2/providers/ecb/rates?base=usd&quotes=cny");
  for (const value of [
    [], [{ date: "2026-11-10", base: "USD", quote: "CNY", rate: 0 }],
    [{ date: "2026-11-10", base: "EUR", quote: "CNY", rate: 1 }],
    [{ date: "not-a-date", base: "USD", quote: "CNY", rate: 1 }],
    [{ date: "2026-02-29", base: "USD", quote: "CNY", rate: 1 }],
    [{ date: "2026-11-10", base: "USD", quote: "CNY", rate: "1e10" }],
    { date: "2026-11-10", base: "USD", quote: "CNY", rate: 1 },
  ]) assert.throws(() => parseRate(value, "USD", "CNY", 0), ReadError);
});

test("currency codes and amounts are validated without accepting exponential or unsafe input", () => {
  assert.equal(currency(" usd "), "USD");
  for (const value of ["BTC", "US", "USD\n", "", 1]) assert.throws(() => currency(value), ReadError);
  assert.equal(amount("001.2300"), "1.2300");
  for (const value of ["", "-1", "1e3", "NaN", "1.2.3", "1\n", "1.1234567890123456789"]) assert.throws(() => amount(value), ReadError);
});

test("decimal multiplication does not introduce binary floating-point rounding", () => {
  assert.equal(multiply("0.1", "0.2"), "0.02");
  assert.equal(multiply("19.99", "6.6703"), "133.339297");
  assert.equal(multiply("0.000000000000000001", "1000000000000000000"), "1");
});

test("local conversions cover common units and temperature offsets", () => {
  assert.equal(unitConversion("1", "mi", "km").result, "1.609344");
  assert.equal(unitConversion("1", "lb", "kg").result, "0.45359237");
  assert.equal(unitConversion("32", "F", "C").result, "0");
  assert.equal(unitConversion("100", "C", "F").result, "212");
  assert.equal(unitConversion("0", "C", "K").result, "273.15");
  assert.equal(unitConversion("1", "GiB", "MiB").result, "1024");
  assert.equal(unitConversion("1", "GB", "MB").result, "1000");
  assert.equal(unitConversion("2", "week", "d").result, "14");
  assert.equal(unit("英尺").symbol, "ft");
  assert.equal(availableUnits("length").includes("mi"), true);
  assert.throws(() => unitConversion("1", "kg", "m"), ReadError);
  assert.throws(() => unitConversion("1", "BTC", "USD"), ReadError);
});

test("state only permits validated conversion targets and local unit selections", () => {
  assert.deepEqual(readState(undefined), initialState);
  const searched = transition(initialState, "convert", { amount: "100", from: "usd", to: "cny" });
  assert.equal(searched.converted, true);
  assert.equal(readState(encodeState(searched)).from, "USD");
  const local = transition(transition(initialState, "mode:units"), "convert", { amount: "2", from: "km", to: "mi" });
  assert.equal(local.converted, true);
  assert.equal(readState(encodeState(local)).from, "km");
  for (const values of [{ amount: "-1", from: "USD", to: "CNY" }, { amount: "1", from: "USD\n", to: "CNY" }, { amount: "1", from: "BTC", to: "CNY" }, { amount: "1", from: "USD", to: "CNY", extra: "ignored" }]) {
    assert.equal(transition(initialState, "convert", values).converted, values.extra === "ignored");
  }
  assert.equal(readState('{"mode":"units","amount":"1","from":"kg","to":"m","dimension":"length","converted":true}').converted, false);
  assert.equal(transition(initialState, "unknown", { from: "BTC" }).converted, false);
});

test("zero fractional rates are rejected, terminating decimals round half-up, and display/clip/page handle edge cases", () => {
  // Zero exchange rates with fractional zeros must be rejected.
  for (const zero of ["0.0", "0.00", "00.000000"]) {
    assert.throws(
      () => parseRate([{ date: "2026-11-10", base: "USD", quote: "CNY", rate: zero }], "USD", "CNY", 0),
      (error: unknown) => error instanceof ReadError && error.code === "bad_answer",
      zero,
    );
  }

  // Non-recurring terminating decimals must round half-up instead of truncating.
  assert.equal(roundedNumber("1.1234567", 6), "1.123457");
  assert.equal(roundedNumber("1.1234565", 6), "1.123457");
  assert.equal(roundedNumber("0.9999999", 6), "1");
  assert.equal(roundedNumber("-0.0000004", 6), "0");

  // formatForDisplay must not throw on invalid locale.
  assert.doesNotThrow(() => formatForDisplay("1234.5678909", "bad_locale_!!!"));
  assert.equal(formatForDisplay("1234.5678909", "en"), "1,234.567891");

  // clip must count Unicode code points and stay <= most.
  assert.equal(clip("abcdef", 0), "");
  assert.equal(clip("abcdef", 1), "a");
  assert.equal(clip("abcdef", 4), "abc…");
  assert.equal(Array.from(clip("😀😀😀😀😀", 4)).length, 4);

  // Lone surrogates must be rejected.
  assert.throws(() => currency("U\ud800D"), ReadError);

  // Direct page() call with invalid state must sanitize safely.
  const service = new CurrencyService(async () => new Response("[]", { status: 200 }));
  const renderer = new Renderer({ authorize: () => false });
  try {
    const view = renderer.render(page({
      service,
      state: { mode: "currency", amount: "-5", from: "INVALID", to: "CNY", dimension: "length", converted: true },
      loaded: { kind: "loading" },
      locale: "en",
    })).view;
    assert.equal(JSON.parse(view.state ?? "{}").converted, false);
  } finally { renderer.dispose(); }
});
