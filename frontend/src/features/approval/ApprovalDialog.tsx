import type { PendingApproval } from "../../state/approval";
import { useStore } from "../../state/store";
import { Button } from "../../ui/Button";
import { Dialog } from "../../ui/Dialog";

/** Full path of the file when Go gave us the folder; otherwise the folder name and relative path. */
function destinationPath(req: PendingApproval): string {
  const rel = req.destRelPath ?? "";
  const root = req.workspacePath;
  if (!root) return req.workspaceName ? `${req.workspaceName} / ${rel}` : rel;
  const sep = root.includes("\\") && !root.includes("/") ? "\\" : "/";
  return root.endsWith(sep) ? root + rel : root + sep + rel;
}

function formatBytes(n = 0): string {
  if (n < 1024) return `${n} bytes`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

/** Esc, click-outside and the close path all mean Deny. Nothing here can approve by accident:
 *  focus starts on Deny and only the explicit Allow buttons send an allow decision. */
export function ApprovalDialog() {
  const req = useStore((s) => s.approval);
  const respond = useStore((s) => s.respondApproval);
  if (!req) return null;

  const who = req.botName || "Faro";
  const title =
    req.kind === "command"
      ? `${who} wants to run a command`
      : req.kind === "file_overwrite"
        ? `${who} wants to replace a file`
        : `${who} wants to save a new file`;

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) void respond("deny");
      }}
      title={title}
      description="Review this request. Nothing happens until you choose."
      onOpenAutoFocus={(e) => {
        e.preventDefault();
        document.querySelector<HTMLElement>("[data-autofocus]")?.focus();
      }}
    >
      <div className="space-y-3 text-sm">
        {req.kind === "command" ? <CommandBody req={req} /> : <FileBody req={req} />}
        <div className="flex flex-wrap justify-end gap-2 pt-1">
          <Button data-autofocus onClick={() => void respond("deny")}>
            Deny
          </Button>
          {req.kind === "command" ? (
            <>
              {req.sandboxed ? (
                <Button onClick={() => void respond("allow_session")}>Allow commands until Faro restarts</Button>
              ) : null}
              <Button variant="danger" onClick={() => void respond("allow")}>
                Allow once
              </Button>
            </>
          ) : req.kind === "file_overwrite" ? (
            <Button variant="danger" onClick={() => void respond("allow")}>
              Replace this file
            </Button>
          ) : (
            <>
              <Button onClick={() => void respond("allow_session")}>
                Allow new files in this folder until Faro restarts
              </Button>
              <Button variant="primary" onClick={() => void respond("allow")}>
                Allow once
              </Button>
            </>
          )}
        </div>
      </div>
    </Dialog>
  );
}

function CommandBody({ req }: { req: PendingApproval }) {
  return (
    <>
      {req.sandboxed ? (
        <div className="inline-block rounded border border-line bg-surface-2 px-2 py-0.5 text-xs font-medium">
          Sandboxed
        </div>
      ) : (
        <div role="alert" className="rounded-ui border border-danger/50 bg-danger/10 px-3 py-2 text-danger">
          <div className="font-medium">
            Runs with your full user permissions and network access. Faro does not restrict it.
          </div>
          <div className="mt-1 text-xs">Only approve this if you trust exactly what it does.</div>
        </div>
      )}
      <pre className="max-h-48 overflow-auto rounded bg-surface-2 p-2 text-xs whitespace-pre-wrap break-all">
        $ {req.command}
      </pre>
      {req.cwd ? <div className="text-xs text-muted">Working folder: {req.cwd}</div> : null}
    </>
  );
}

function FileBody({ req }: { req: PendingApproval }) {
  const overwrite = req.kind === "file_overwrite";
  return (
    <>
      <div>
        <div className="text-xs font-medium text-muted">{overwrite ? "Replaces" : "Saves to"}</div>
        <div className="mt-1 rounded bg-surface-2 p-2 text-xs break-all">
          {destinationPath(req)}
        </div>
      </div>
      <div className="text-xs text-muted">
        {overwrite
          ? `New size ${formatBytes(req.sizeBytes)} (currently ${formatBytes(req.existingSizeBytes)}). The original is backed up first.`
          : `Size ${formatBytes(req.sizeBytes)}`}
      </div>
      {req.preview ? (
        <div>
          <div className="text-xs font-medium text-muted">Start of the content</div>
          <pre className="mt-1 max-h-32 overflow-auto rounded bg-surface-2 p-2 text-xs whitespace-pre-wrap break-words">
            {req.preview}
          </pre>
        </div>
      ) : null}
    </>
  );
}
