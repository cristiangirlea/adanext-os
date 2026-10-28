import type { AttemptEvent } from "../client/api";
import { Marked } from "../text/Marked";

// An attempt's events rendered by kind (internal/api doc.go, "Event kinds"), not as raw JSON.
// An engine event's payload is the protocol line, its content under "event"; executor, store and
// result events carry their own small objects. Everything is text: nothing an agent wrote is
// ever HTML. What a kind does not name is kept in a closed "Raw" disclosure.

type Line = { kind: string; tone: "agent" | "tool" | "quiet" | "good" | "bad" | "warn"; label: string; text: string; extra?: string };

function obj(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : {};
}
function str(value: unknown): string {
  if (typeof value === "string") return value;
  if (value === undefined || value === null) return "";
  return JSON.stringify(value) ?? "";
}
function clip(text: string, max = 4000): string {
  return text.length > max ? `${text.slice(0, max)}…` : text;
}

export function describeEvent(event: AttemptEvent): Line {
  const payload = obj(event.payload);
  const inner = obj(payload.event);
  const raw = clip(str(event.payload), 2000);
  switch (event.kind) {
    case "text":
      return { kind: event.kind, tone: "agent", label: "Agent", text: clip(str(inner.text ?? event.payload)) };
    case "thinking":
      return { kind: event.kind, tone: "quiet", label: "Thinking", text: clip(str(inner.text ?? "")) };
    case "tool_call": {
      const tool = str(inner.tool ?? inner.name ?? "tool");
      return { kind: event.kind, tone: "tool", label: `Tool ${tool}`, text: clip(str(inner.text ?? inner.input ?? "")), extra: raw };
    }
    case "tool_result":
      return { kind: event.kind, tone: "tool", label: "Tool result", text: clip(str(inner.text ?? inner.output ?? "")), extra: raw };
    case "info":
      return { kind: event.kind, tone: "quiet", label: "Info", text: clip(str(inner.text ?? payload.text ?? "")) };
    case "session":
      return { kind: event.kind, tone: "quiet", label: "Session", text: clip(str(inner.text ?? inner.session ?? "")), extra: raw };
    case "error":
      return { kind: event.kind, tone: "bad", label: "Engine error", text: clip(str(inner.text ?? payload.error ?? "")), extra: raw };
    case "engine_notice": {
      const error = obj(payload.error);
      const fatal = payload.fatal === true;
      return { kind: event.kind, tone: fatal ? "bad" : "warn", label: fatal ? "Engine stopped" : "Engine notice", text: [str(error.code), str(error.message)].filter(Boolean).join(": ") || raw };
    }
    case "result": {
      const result = obj(payload.result);
      const status = str(result.status) || "ended";
      return { kind: event.kind, tone: status === "ok" ? "good" : "bad", label: "Result", text: [status, clip(str(result.final ?? ""), 800)].filter(Boolean).join(": "), extra: raw };
    }
    case "executor_error":
      return { kind: event.kind, tone: "bad", label: "Ada stopped it", text: str(payload.error) || raw };
    case "recovery":
      return { kind: event.kind, tone: "warn", label: "Recovery needed", text: str(payload.reason) || raw, extra: raw };
    case "cancel":
      return { kind: event.kind, tone: "warn", label: "Canceled", text: str(payload.reason) || raw };
    default:
      return { kind: event.kind, tone: "quiet", label: event.kind, text: raw };
  }
}

export function Activity({ events, stream, hasAttempt }: { events: AttemptEvent[]; stream: string; hasAttempt: boolean }) {
  return (
    <section className="activity" aria-labelledby="activity-title">
      <h3 id="activity-title">
        Activity {stream && <span className="stream">{stream}</span>}
      </h3>
      {!hasAttempt ? (
        <p className="empty">Nothing has run yet. Mark the task ready, then start it.</p>
      ) : events.length === 0 ? (
        <p className="empty">No events yet.</p>
      ) : (
        <ol className="events" aria-label="Events">
          {events.map((event) => {
            const line = describeEvent(event);
            return (
              <li key={event.seq} data-seq={event.seq} data-tone={line.tone}>
                <span className="seq">{event.seq}</span>
                <span className="kind" title={line.label}>
                  {event.kind}
                </span>
                <span className="event-body">
                  {line.label !== event.kind && <span className="event-label">{line.label}</span>}
                  <span className="event-text">
                    <Marked text={line.text} keepNewlines />
                  </span>
                  {line.extra && (
                    <details className="event-raw">
                      <summary>Raw</summary>
                      <code>
                        <Marked text={line.extra} />
                      </code>
                    </details>
                  )}
                </span>
              </li>
            );
          })}
        </ol>
      )}
    </section>
  );
}
