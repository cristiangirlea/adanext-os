import { useRef, type KeyboardEvent, type PointerEvent } from "react";
import { effectAttrs } from "../motion/effects";

// The one resize handle, shared by the work-area panes and the drawers. Four edges (n, e, s, w)
// and four corners; the handle sits on that side of the pane it resizes. Arrow keys move the
// edge the way they point by a step (Shift: four steps), Home and End go to the limits.
export const sides = ["n", "e", "s", "w", "ne", "nw", "se", "sw"] as const;
export type Side = (typeof sides)[number];
export interface Size {
  width: number;
  height: number;
}

interface Props {
  side: Side;
  size: Size;
  min: Size;
  max: Size;
  onResize: (size: Size) => void;
  label: string;
  controls?: string;
  step?: number;
}

const clamp = (value: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, value));

// How the pane grows per pixel the edge moves right (x) and down (y): +1, -1, or 0 when the side
// does not touch that axis.
function signs(side: Side): { x: number; y: number } {
  return {
    x: side.includes("e") ? 1 : side.includes("w") ? -1 : 0,
    y: side.includes("s") ? 1 : side.includes("n") ? -1 : 0,
  };
}

export function ResizeHandle({ side, size, min, max, onResize, label, controls, step = 16 }: Props) {
  const drag = useRef<{ x: number; y: number; size: Size; pointer: number } | null>(null);
  const { x, y } = signs(side);
  const corner = x !== 0 && y !== 0;

  const resize = (width: number, height: number) => {
    const next = { width: x ? clamp(width, min.width, max.width) : size.width, height: y ? clamp(height, min.height, max.height) : size.height };
    if (next.width !== size.width || next.height !== size.height) onResize(next);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const by = e.shiftKey ? step * 4 : step;
    const moves: Record<string, [number, number]> = { ArrowRight: [by, 0], ArrowLeft: [-by, 0], ArrowDown: [0, by], ArrowUp: [0, -by] };
    const move = moves[e.key];
    if (move) {
      const [dx, dy] = move;
      if ((dx !== 0 && x === 0) || (dy !== 0 && y === 0)) return;
      e.preventDefault();
      resize(size.width + dx * x, size.height + dy * y);
    } else if (e.key === "Home") {
      e.preventDefault();
      resize(min.width, min.height);
    } else if (e.key === "End") {
      e.preventDefault();
      resize(max.width, max.height);
    }
  };

  const onPointerDown = (e: PointerEvent<HTMLDivElement>) => {
    if (e.button !== 0) return;
    // No text selection while dragging; focus moves here so the keys work after a click.
    e.preventDefault();
    e.currentTarget.focus();
    drag.current = { x: e.clientX, y: e.clientY, size, pointer: e.pointerId };
    e.currentTarget.setPointerCapture?.(e.pointerId);
  };
  const onPointerMove = (e: PointerEvent<HTMLDivElement>) => {
    const start = drag.current;
    if (!start || start.pointer !== e.pointerId) return;
    resize(start.size.width + (e.clientX - start.x) * x, start.size.height + (e.clientY - start.y) * y);
  };
  const end = (e: PointerEvent<HTMLDivElement>) => {
    if (drag.current?.pointer !== e.pointerId) return;
    drag.current = null;
    e.currentTarget.releasePointerCapture?.(e.pointerId);
  };

  // An edge between panes side by side is a vertical line; between stacked panes, horizontal.
  // A corner reports its width as the value and both sides in words.
  const vertical = x !== 0;
  const now = vertical ? size.width : size.height;
  return (
    <div
      role="separator"
      tabIndex={0}
      aria-label={label}
      aria-controls={controls}
      aria-orientation={corner ? undefined : vertical ? "vertical" : "horizontal"}
      aria-valuenow={now}
      aria-valuemin={vertical ? min.width : min.height}
      aria-valuemax={vertical ? max.width : max.height}
      aria-valuetext={corner ? `${size.width} by ${size.height} pixels` : undefined}
      className="resize-handle"
      data-side={side}
      {...effectAttrs("handle")}
      onKeyDown={onKeyDown}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={end}
      onPointerCancel={end}
    />
  );
}
