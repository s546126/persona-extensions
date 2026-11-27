import { callback, useLoadEffect, useState, type Node } from "@persona/extension/ui";
import { encodeState, loadPage, page, readState, transition, type Loaded } from "./page.tsx";
import type { Wiktionary } from "./service.ts";
export interface RootOptions { service: Wiktionary; locale?: string; tz?: string; uiVersion?: number; state?: unknown }
export function Root(options: RootOptions): Node {
  const [view, setView] = useState(() => ({ state: readState(options.state), refresh: 0 }));
  const { state, refresh } = view;
  const [result, setResult] = useState<{ service: Wiktionary; identity: string; loaded: Loaded } | undefined>(undefined);
  const identity = JSON.stringify([state.q, state.found, refresh]);
  useLoadEffect(async signal => {
    const loaded = await loadPage(options.service, state, refresh > 0, signal);
    if (!signal.aborted) setResult(previous => previous?.service === options.service && previous.identity === identity && JSON.stringify(previous.loaded) === JSON.stringify(loaded)
      ? previous : { service: options.service, identity, loaded });
  }, [options.service, identity]);
  return page({ ...options, state, loaded: result?.service === options.service && result.identity === identity ? result.loaded : { kind: "loading" },
    action: (id, next, title) => {
      if (next && (options.uiVersion ?? 0) >= 3) {
        const push = { ui: "wiktionary", state: encodeState(next), ...(title ? { title } : {}) };
        return callback(() => ({ push }), { push });
      }
      return callback(values => {
        if (id === "refresh") setView(previous => ({ state: previous.state, refresh: previous.refresh + 1 }));
        else setView(previous => {
          const nextState = transition(previous.state, id, values);
          return nextState.q === previous.state.q && nextState.found === previous.state.found ? previous : { state: nextState, refresh: 0 };
        });
      }, { submit: true });
    } });
}
export function root(options: RootOptions): Node { return <Root key="wiktionary-root" {...options} />; }
