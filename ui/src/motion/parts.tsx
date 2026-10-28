import type { ButtonHTMLAttributes } from "react";
import { effectAttrs, useEffectMode } from "./effects";

// A placeholder shaped like the rows it stands for, while they load.
export function Skeleton({ rows, label }: { rows: number; label: string }) {
  const shimmer = useEffectMode("shimmer") === "run";
  return (
    <div className="skeleton" role="status" aria-label={label} aria-busy="true">
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="skeleton-row" aria-hidden="true">
          <span className="skeleton-bar skeleton-wide" />
          <span className="skeleton-bar skeleton-narrow" />
          <span className="skeleton-bar skeleton-chip" />
        </div>
      ))}
      {shimmer && <span className="skeleton-shimmer" aria-hidden="true" {...effectAttrs("shimmer")} />}
    </div>
  );
}

// The spinner for a button just pressed; the button itself carries aria-busy.
export function Spinner() {
  const mode = useEffectMode("spinner");
  return <span className="spinner" aria-hidden="true" data-mode={mode} {...effectAttrs("spinner")} />;
}

// A button that presses in (the press effect) and, while the request it started runs, says so
// with aria-busy and a spinner.
export function BusyButton({ busy, children, ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { busy: boolean }) {
  return (
    <button type="button" {...props} {...effectAttrs("press")} aria-busy={busy || undefined}>
      {children}
      {busy && <Spinner />}
    </button>
  );
}
