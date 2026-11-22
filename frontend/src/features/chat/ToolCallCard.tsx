import type { Part } from "../../state/chat";
import { Spinner } from "../../ui/Spinner";
import { cn } from "../../ui/cn";
import { prettyJson, toolLabel, toolSummary } from "./toolLabels";

type ToolPart = Extract<Part, { kind: "tool" }>;

export function ToolCallCard({ part }: { part: ToolPart }) {
  const summary = toolSummary(part.args);
  const failed = part.status === "error";
  const declined = part.status === "declined";
  return (
    <details className="group rounded-ui border border-line bg-surface text-sm">
      <summary className="flex cursor-pointer list-none items-center gap-2 px-3 py-2">
        <span className={cn("flex w-4 justify-center", failed ? "text-danger" : declined ? "text-muted" : "text-ok")}>
          {part.status === "running" ? <Spinner className="text-accent" /> : failed ? "!" : declined ? "–" : "✓"}
        </span>
        <span className="font-medium">{toolLabel(part.name, part.status)}</span>
        {summary ? <span className="truncate text-muted">{summary}</span> : null}
        {failed ? <span className="ml-auto shrink-0 text-xs text-danger">didn't work</span> : null}
        {declined ? <span className="ml-auto shrink-0 text-xs text-muted">You declined</span> : null}
        {part.durationMs ? (
          <span className={cn("shrink-0 text-xs text-muted", !failed && !declined && "ml-auto")}>{part.durationMs} ms</span>
        ) : null}
      </summary>
      <div className="space-y-2 border-t border-line px-3 py-2">
        <Detail label="Request" body={prettyJson(part.args)} />
        {part.result !== undefined ? <Detail label="Result" body={prettyJson(part.result)} /> : null}
      </div>
    </details>
  );
}

function Detail({ label, body }: { label: string; body: string }) {
  return (
    <div>
      <div className="text-xs font-medium text-muted">{label}</div>
      <pre className="mt-1 max-h-60 overflow-auto rounded bg-surface-2 p-2 text-xs whitespace-pre-wrap">{body}</pre>
    </div>
  );
}
