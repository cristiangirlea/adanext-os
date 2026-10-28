import { AdaError } from "../client/api";

export interface Shown {
  code: string;
  message: string;
  requestId?: string;
}

const described: Record<string, string> = {
  coordinator_unreachable: "coordinator unreachable: adad is not running, or the shell was not told its pipe or socket",
  bridge_unavailable: "this page is not running in the Ada desktop shell",
  bad_response: "the coordinator answered with something the shell could not read",
  folder_expired: "the folder choice has expired; choose the folder again",
  path_refused: "a folder can only be chosen in the desktop's own dialog",
};

export function shown(error: unknown): Shown {
  if (error instanceof AdaError) return { code: error.code, message: described[error.code] ?? error.message, requestId: error.requestId };
  return { code: "internal", message: error instanceof Error ? error.message : String(error) };
}

// One refusal, with adad's code and, for an internal error, the request id to match its log.
// alert is false for a refusal shown inside a pane, so the page holds one alert at a time.
export function ErrorLine({ error, alert = true }: { error: Shown; alert?: boolean }) {
  return (
    <p role={alert ? "alert" : undefined} className="error">
      <code>{error.code}</code> {error.message}
      {error.requestId && (
        <>
          {" "}
          (request <code>{error.requestId}</code>)
        </>
      )}
    </p>
  );
}
