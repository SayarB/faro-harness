import { useStore } from "../../state/store";
import { Button } from "../../ui/Button";
import { Spinner } from "../../ui/Spinner";

export function FolderList() {
  const workspaces = useStore((s) => s.workspaces);
  const index = useStore((s) => s.index);
  const addFolder = useStore((s) => s.addFolder);
  const rescan = useStore((s) => s.rescan);

  return (
    <section className="space-y-2">
      <div className="flex items-center justify-between">
        <h2 className="text-xs font-semibold tracking-wide text-muted uppercase">Folders</h2>
        <Button variant="ghost" className="px-2 py-0.5 text-xs" onClick={() => void addFolder()}>
          + Add
        </Button>
      </div>
      {workspaces.length === 0 ? (
        <p className="text-xs text-muted">Add a folder so Faro can read the files in it.</p>
      ) : (
        <ul className="space-y-1">
          {workspaces.map((ws) => {
            const p = index[ws.id];
            const busy = p !== undefined && !p.done;
            return (
              <li key={ws.id} className="rounded-ui px-2 py-1.5 hover:bg-surface-2" title={ws.path}>
                <div className="flex items-center gap-2 text-sm">
                  <span className="truncate font-medium">{ws.name}</span>
                  {busy ? (
                    <Spinner className="ml-auto text-accent" />
                  ) : (
                    <button
                      className="ml-auto text-xs text-muted hover:text-fg"
                      onClick={() => void rescan(ws.id)}
                      aria-label={`Rescan ${ws.name}`}
                    >
                      Rescan
                    </button>
                  )}
                </div>
                <div className="text-xs text-muted">
                  {busy
                    ? `Reading files… ${p.indexed} new, ${p.skipped} unchanged`
                    : p?.error
                      ? <span className="text-danger">Couldn't finish: {p.error}</span>
                      : `${ws.fileCount} files ready`}
                </div>
              </li>
            );
          })}
        </ul>
      )}
      <p className="text-xs text-muted">Faro can read every file in the folders you add.</p>
    </section>
  );
}
