import { useLayoutEffect, useRef, type ReactNode } from "react";

export function RootDialog({ title, children, onClose }: { title: string; children: ReactNode; onClose: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  useLayoutEffect(() => {
    const el = dialog.current!;
    if (typeof el.showModal === "function") el.showModal();
    else el.setAttribute("open", "");
    return () => { if (el.open && typeof el.close === "function") el.close(); };
  }, []);
  return (
    <dialog className="import" ref={dialog} aria-labelledby="root-dialog-title" onCancel={(e) => { e.preventDefault(); onClose(); }}>
      <header className="import-head">
        <h2 id="root-dialog-title">{title}</h2>
        <button type="button" className="quiet" onClick={onClose}>Close</button>
      </header>
      <div className="import-body">{children}</div>
    </dialog>
  );
}
