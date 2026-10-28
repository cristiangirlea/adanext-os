import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it } from "vitest";
import { ResizeHandle, sides, type Side, type Size } from "./ResizeHandle";

afterEach(cleanup);

const resizeCss = readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "styles", "resize.css"), "utf8");

const min = { width: 200, height: 100 };
const max = { width: 600, height: 400 };

function Pane({ side, start = { width: 300, height: 200 } }: { side: Side; start?: Size }) {
  const [size, setSize] = useState(start);
  return (
    <div id="pane">
      <output data-testid="size">{`${size.width}x${size.height}`}</output>
      <ResizeHandle side={side} size={size} min={min} max={max} step={10} onResize={setSize} label="Resize the pane" controls="pane" />
    </div>
  );
}

const handle = () => screen.getByRole("separator", { name: "Resize the pane" });
const size = () => screen.getByTestId("size").textContent;

describe("ResizeHandle", () => {
  it("has four edges and four corners", () => {
    expect([...sides].sort()).toEqual(["e", "n", "ne", "nw", "s", "se", "sw", "w"]);
  });

  it("is a focusable separator with its value, limits and orientation", () => {
    render(<Pane side="e" />);
    const h = handle();
    expect(h.tabIndex).toBe(0);
    expect(h.getAttribute("aria-orientation")).toBe("vertical");
    expect(h.getAttribute("aria-valuenow")).toBe("300");
    expect(h.getAttribute("aria-valuemin")).toBe("200");
    expect(h.getAttribute("aria-valuemax")).toBe("600");
    expect(h.getAttribute("aria-controls")).toBe("pane");
    cleanup();
    render(<Pane side="s" />);
    expect(handle().getAttribute("aria-orientation")).toBe("horizontal");
    expect(handle().getAttribute("aria-valuenow")).toBe("200");
  });

  it("resizes by a step with the arrow keys along its own axis, and moves the edge the way the arrow points", () => {
    render(<Pane side="e" />);
    fireEvent.keyDown(handle(), { key: "ArrowRight" });
    expect(size()).toBe("310x200");
    fireEvent.keyDown(handle(), { key: "ArrowLeft" });
    fireEvent.keyDown(handle(), { key: "ArrowLeft" });
    expect(size()).toBe("290x200");
    fireEvent.keyDown(handle(), { key: "ArrowUp" });
    expect(size()).toBe("290x200");
    cleanup();
    render(<Pane side="w" />);
    fireEvent.keyDown(handle(), { key: "ArrowLeft" });
    expect(size()).toBe("310x200");
    cleanup();
    render(<Pane side="n" />);
    fireEvent.keyDown(handle(), { key: "ArrowUp" });
    expect(size()).toBe("300x210");
  });

  it("goes to the limits with Home and End and never past them", () => {
    render(<Pane side="e" />);
    fireEvent.keyDown(handle(), { key: "End" });
    expect(size()).toBe("600x200");
    fireEvent.keyDown(handle(), { key: "ArrowRight" });
    expect(size()).toBe("600x200");
    fireEvent.keyDown(handle(), { key: "Home" });
    expect(size()).toBe("200x200");
    fireEvent.keyDown(handle(), { key: "ArrowLeft" });
    expect(size()).toBe("200x200");
  });

  it("resizes both axes from a corner, by keyboard and to the limits", () => {
    render(<Pane side="se" />);
    expect(handle().getAttribute("aria-valuetext")).toBe("300 by 200 pixels");
    fireEvent.keyDown(handle(), { key: "ArrowRight" });
    fireEvent.keyDown(handle(), { key: "ArrowDown" });
    expect(size()).toBe("310x210");
    fireEvent.keyDown(handle(), { key: "End" });
    expect(size()).toBe("600x400");
    fireEvent.keyDown(handle(), { key: "Home" });
    expect(size()).toBe("200x100");
  });

  it("resizes with the mouse, clamped to the limits", () => {
    render(<Pane side="e" />);
    fireEvent.pointerDown(handle(), { clientX: 100, clientY: 0, button: 0, pointerId: 1 });
    expect(document.activeElement).toBe(handle());
    fireEvent.pointerMove(handle(), { clientX: 150, clientY: 40, pointerId: 1 });
    expect(size()).toBe("350x200");
    fireEvent.pointerMove(handle(), { clientX: 1000, clientY: 0, pointerId: 1 });
    expect(size()).toBe("600x200");
    fireEvent.pointerMove(handle(), { clientX: -1000, clientY: 0, pointerId: 1 });
    expect(size()).toBe("200x200");
    fireEvent.pointerUp(handle(), { clientX: -1000, clientY: 0, pointerId: 1 });
    fireEvent.pointerMove(handle(), { clientX: 400, clientY: 0, pointerId: 1 });
    expect(size()).toBe("200x200");
  });

  it("resizes a west or north-west handle against the pointer", () => {
    render(<Pane side="nw" />);
    fireEvent.pointerDown(handle(), { clientX: 100, clientY: 100, button: 0, pointerId: 1 });
    fireEvent.pointerMove(handle(), { clientX: 80, clientY: 70, pointerId: 1 });
    expect(size()).toBe("320x230");
    fireEvent.pointerUp(handle(), { pointerId: 1 });
  });

  it("ignores buttons other than the main one", () => {
    render(<Pane side="e" />);
    fireEvent.pointerDown(handle(), { clientX: 100, clientY: 0, button: 2, pointerId: 1 });
    fireEvent.pointerMove(handle(), { clientX: 150, clientY: 0, pointerId: 1 });
    expect(size()).toBe("300x200");
  });

  it("gives edges a 6 px hit area and corners 12 px", () => {
    const text = resizeCss;
    expect(text).toMatch(/--resize-edge:\s*6px/);
    expect(text).toMatch(/--resize-corner:\s*12px/);
    render(<Pane side="ne" />);
    expect(handle().dataset.side).toBe("ne");
    expect(handle().classList.contains("resize-handle")).toBe(true);
  });
});
