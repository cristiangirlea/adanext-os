import { useSyncExternalStore } from "react";

// The one effect registry. Every effect declares what it does when motion is off (the
// body[data-motion="off"] switch or prefers-reduced-motion): "static" holds its resting frame,
// "off" drops the effect and keeps the content, "unchanged" keeps it (opacity-only feedback that
// does not move). An effect's element carries effectAttrs(name), which motion.css reads; a
// component that renders differently reads useEffectMode(name). No effect switches itself on.
export type Reduced = "static" | "off" | "unchanged";
export type Mode = "run" | "static" | "off";

export const effects = {
  press: { reduced: "off", does: "a button scales down a little while it is pressed" },
  entrance: { reduced: "static", does: "the palette and drawers fade and rise into place" },
  shimmer: { reduced: "off", does: "a light band sweeps across a loading skeleton" },
  spinner: { reduced: "static", does: "a ring turns on the button just pressed while its request runs" },
  handle: { reduced: "unchanged", does: "a resize handle's line fades in on hover and focus" },
} as const satisfies Record<string, { reduced: Reduced; does: string }>;
export type EffectName = keyof typeof effects;

export const motionKey = "ada.motion";
const changed = "ada:motion";
const reduceQuery = "(prefers-reduced-motion: reduce)";

function reducedMedia(): MediaQueryList | null {
  return typeof window.matchMedia === "function" ? window.matchMedia(reduceQuery) : null;
}

export function motionOff(): boolean {
  return document.body.dataset.motion === "off" || reducedMedia()?.matches === true;
}

export function effectMode(name: EffectName): Mode {
  if (!motionOff()) return "run";
  const reduced: Reduced = effects[name].reduced;
  return reduced === "unchanged" ? "run" : reduced;
}

export function effectAttrs(name: EffectName): { "data-effect": EffectName; "data-reduced": Reduced } {
  return { "data-effect": name, "data-reduced": effects[name].reduced };
}

// setMotion(false) is the kill switch: it marks <body> and saves the choice on this device.
export function setMotion(on: boolean): void {
  if (on) delete document.body.dataset.motion;
  else document.body.dataset.motion = "off";
  try {
    if (on) localStorage.removeItem(motionKey);
    else localStorage.setItem(motionKey, "off");
  } catch {
    // Storage refused: the switch still holds for this page.
  }
  window.dispatchEvent(new Event(changed));
}

// restoreMotion applies a saved "off"; called once at startup, it never turns motion off by itself.
export function restoreMotion(): void {
  let saved: string | null = null;
  try {
    saved = localStorage.getItem(motionKey);
  } catch {
    return;
  }
  if (saved === "off") document.body.dataset.motion = "off";
}

function subscribe(onChange: () => void): () => void {
  const media = reducedMedia();
  window.addEventListener(changed, onChange);
  media?.addEventListener?.("change", onChange);
  return () => {
    window.removeEventListener(changed, onChange);
    media?.removeEventListener?.("change", onChange);
  };
}

export function useEffectMode(name: EffectName): Mode {
  return useSyncExternalStore(subscribe, () => effectMode(name));
}

export function useMotionOn(): boolean {
  return useSyncExternalStore(subscribe, () => document.body.dataset.motion !== "off");
}
