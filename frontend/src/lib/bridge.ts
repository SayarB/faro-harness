export type IndexDepth = "metadata_only" | "header_only" | "full";

export interface SearchHit {
  fileId: number;
  absPath: string;
  relPath: string;
  filename: string;
  folder: string;
  ext: string;
  size: number;
  mtimeUnixNs: number;
  indexDepth: IndexDepth;
  snippet: string;
  score: number;
}

export interface IndexStats {
  totalFiles: number;
  totalBlobs: number;
  byDepth: Record<string, number>;
  workspaces: string[];
}

export interface IndexProgress {
  workspace: string;
  scanned: number;
  indexed: number;
  skipped: number;
  errors: number;
  done: boolean;
}

interface WailsAppBindings {
  PickWorkspace: () => Promise<IndexProgress>;
  IndexWorkspace: (root: string) => Promise<IndexProgress>;
  Search: (query: string, limit: number) => Promise<SearchHit[]>;
  Stats: () => Promise<IndexStats>;
}

interface WailsRuntime {
  EventsOn?: (eventName: string, cb: (data: unknown) => void) => () => void;
}

declare global {
  interface Window {
    go?: { main?: { App?: WailsAppBindings } };
    runtime?: WailsRuntime;
  }
}

function getApp(): WailsAppBindings | undefined {
  return window.go?.main?.App;
}

export const isDesktopRuntime = (): boolean => Boolean(getApp());

export async function pickWorkspace(): Promise<IndexProgress | null> {
  const app = getApp();
  if (!app) return null;
  return app.PickWorkspace();
}

export async function searchFiles(query: string, limit = 30): Promise<SearchHit[]> {
  const app = getApp();
  if (!app) return [];
  return app.Search(query, limit);
}

export async function fetchStats(): Promise<IndexStats> {
  const app = getApp();
  if (!app) {
    return { totalFiles: 0, totalBlobs: 0, byDepth: {}, workspaces: [] };
  }
  return app.Stats();
}

export function onIndexEvent(
  eventName: "index:progress" | "index:updated",
  cb: (payload: unknown) => void,
): () => void {
  const eventsOn = window.runtime?.EventsOn;
  if (!eventsOn) return () => {};
  return eventsOn(eventName, cb);
}
