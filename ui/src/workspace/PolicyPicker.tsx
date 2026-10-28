import { useId } from "react";
import type { Capabilities, ExecutionPolicy } from "../client/api";
import { cardRows, claim, modeOrder, rowNames, timeLimits, turnLimits, withLimit, yoloDisables } from "./policy";

// Backend, model, effort and mode, all from the capability matrix. Each mode shows what it
// actually holds for the chosen backend, cell by cell, so "readonly" never reads as a sandbox
// when the matrix says it is only asked of the CLI.
export function PolicyPicker({ matrix, value, onChange, legend = "Agent" }: { matrix: Capabilities | null; value: ExecutionPolicy; onChange: (next: ExecutionPolicy) => void; legend?: string }) {
  const id = useId();
  const backends = matrix?.backends ?? [];
  const backend = backends.find((b) => b.backend === value.backend);
  const modes = backend?.modes ?? modeOrder.map((mode) => ({ mode, offered: true, cells: undefined, reason: undefined }));
  const modeOf = (mode: string) => modes.find((m) => m.mode === mode);

  return (
    <fieldset className="policy">
      <legend>{legend}</legend>
      <div className="policy-selects">
        <label>
          Backend
          <select
            value={value.backend}
            // A backend change starts again from readonly: no other mode carries over.
            onChange={(e) => onChange({ backend: e.target.value, model: "", effort: "", mode: "readonly" })}
          >
            {backends.length === 0 && <option value={value.backend}>{value.backend}</option>}
            {backends.map((b) => (
              <option key={b.backend} value={b.backend}>
                {b.backend}
                {b.installed && !b.installed.available ? " (not installed)" : ""}
              </option>
            ))}
          </select>
        </label>
        <label>
          Model
          <select value={value.model} onChange={(e) => onChange({ ...value, model: e.target.value })}>
            <option value="">The CLI's default</option>
            {(backend?.models ?? []).map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
        </label>
        <label>
          Effort
          <select value={value.effort} onChange={(e) => onChange({ ...value, effort: e.target.value })}>
            <option value="">The CLI's default</option>
            {(backend?.efforts ?? []).map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
        </label>
        {backend?.options.includes("timeout") && (
          <label>
            Time limit
            <select value={value.timeout ?? ""} onChange={(e) => onChange(withLimit(value, "timeout", e.target.value))}>
              <option value="">None</option>
              {timeLimits.map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </select>
          </label>
        )}
        {backend?.options.includes("max_turns") && (
          <label>
            Turn limit
            <select value={value.max_turns === undefined ? "" : String(value.max_turns)} onChange={(e) => onChange(withLimit(value, "max_turns", e.target.value))}>
              <option value="">None</option>
              {turnLimits.map((t) => (
                <option key={t} value={String(t)}>
                  {t}
                </option>
              ))}
            </select>
          </label>
        )}
      </div>
      {backend?.installed && (
        <p className="policy-cli">
          {backend.backend} {backend.installed.available ? backend.installed.version : "is not installed"}; the matrix was recorded against {backend.cli_version} on {backend.observed}.
        </p>
      )}
      <div className="modes" role="radiogroup" aria-label="Mode">
        {modeOrder.map((mode) => {
          const m = modeOf(mode);
          const offered = m?.offered !== false;
          const checked = value.mode === mode;
          return (
            <div key={mode} className={`mode mode-${mode}`} data-checked={checked || undefined}>
              <label className="mode-head">
                <input type="radio" name={`${id}-mode`} value={mode} checked={checked} disabled={!offered} onChange={() => onChange({ ...value, mode })} />
                <span className="mode-name">{mode}</span>
                {mode === "readonly" && <span className="mode-tag">default</span>}
              </label>
              {m?.cells && (
                <dl className="cells">
                  {cardRows.map((row) => {
                    const cell = m.cells?.[row];
                    if (!cell) return null;
                    return (
                      <div key={row} className="cell" data-status={cell.status}>
                        <dt>{rowNames[row]}</dt>
                        <dd>{claim(cell)}</dd>
                      </div>
                    );
                  })}
                </dl>
              )}
              {mode === "yolo" && (
                <div className="yolo-warning">
                  <p>Turns off</p>
                  <ul>
                    {yoloDisables.map((line) => (
                      <li key={line}>{line}</li>
                    ))}
                  </ul>
                </div>
              )}
            </div>
          );
        })}
        {modes
          .filter((m) => !m.offered)
          .map((m) => (
            <p key={m.mode} className="mode-refused">
              {m.mode}: {m.reason}
            </p>
          ))}
      </div>
      {modeOf(value.mode)?.cells && (
        <details className="all-rows">
          <summary>Every row for {value.backend} in {value.mode}</summary>
          <table>
            <tbody>
              {Object.keys(rowNames).map((row) => {
                const cell = modeOf(value.mode)?.cells?.[row];
                if (!cell) return null;
                return (
                  <tr key={row} data-status={cell.status}>
                    <th scope="row">{rowNames[row]}</th>
                    <td>{claim(cell)}</td>
                    <td>{cell.detail}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </details>
      )}
    </fieldset>
  );
}
