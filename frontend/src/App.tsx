import { useCallback, useEffect, useState } from "react";
import {
  fetchStats,
  isDesktopRuntime,
  onIndexEvent,
  pickWorkspace,
  searchFiles,
  type IndexProgress,
  type IndexStats,
  type SearchHit,
} from "./lib/bridge";

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

export function App() {
  const [query, setQuery] = useState("");
  const [hits, setHits] = useState<SearchHit[]>([]);
  const [stats, setStats] = useState<IndexStats>({
    totalFiles: 0,
    totalBlobs: 0,
    byDepth: {},
    workspaces: [],
  });
  const [progress, setProgress] = useState<IndexProgress | null>(null);
  const [indexing, setIndexing] = useState(false);

  const refresh = useCallback(async (q: string) => {
    const [nextStats, nextHits] = await Promise.all([fetchStats(), searchFiles(q, 30)]);
    setStats(nextStats);
    setHits(nextHits);
  }, []);

  useEffect(() => {
    void refresh(query);
  }, [query, refresh]);

  useEffect(() => {
    const offProgress = onIndexEvent("index:progress", (data) => {
      setProgress(data as IndexProgress);
    });
    const offUpdated = onIndexEvent("index:updated", () => {
      void refresh(query);
    });
    return () => {
      offProgress();
      offUpdated();
    };
  }, [query, refresh]);

  const handlePickFolder = async () => {
    setIndexing(true);
    try {
      const res = await pickWorkspace();
      if (res) setProgress(res);
      await refresh(query);
    } finally {
      setIndexing(false);
    }
  };

  return (
    <main className="shell">
      <header className="header">
        <div className="title">
          <h1>Faro Harness</h1>
          <p>Go workers + SQLite FTS5 card catalog + React UI</p>
        </div>
        <button className="button" onClick={handlePickFolder} disabled={indexing || !isDesktopRuntime()}>
          {indexing ? "Indexing…" : "Add Folder"}
        </button>
      </header>

      <section className="stats">
        <span className="pill">
          Files: <strong>{stats.totalFiles}</strong>
        </span>
        <span className="pill">
          Unique Blobs: <strong>{stats.totalBlobs}</strong>
        </span>
        <span className="pill">
          Full: <strong>{stats.byDepth.full ?? 0}</strong>
        </span>
        <span className="pill">
          Header Peek: <strong>{stats.byDepth.header_only ?? 0}</strong>
        </span>
        <span className="pill">
          Metadata Only: <strong>{stats.byDepth.metadata_only ?? 0}</strong>
        </span>
        {progress && !progress.done && (
          <span className="pill">
            Scanning: <strong>{progress.indexed + progress.skipped}/{progress.scanned}</strong>
          </span>
        )}
      </section>

      <input
        className="search-input"
        type="search"
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        placeholder="Search filenames, folders, ZIP contents, Excel sheet names, or text…"
      />

      {!isDesktopRuntime() && (
        <div className="empty">
          Running in browser preview mode. Start via <code>wails dev</code> to connect the Go indexer backend.
        </div>
      )}

      {isDesktopRuntime() && hits.length === 0 && (
        <div className="empty">
          {stats.totalFiles === 0
            ? 'Click "Add Folder" to index a workspace.'
            : "No matching files found."}
        </div>
      )}

      <section className="hits">
        {hits.map((hit) => (
          <article className="hit" key={hit.fileId}>
            <div className="hit-top">
              <span className="hit-name">{hit.filename}</span>
              <span className="hit-depth">
                {hit.indexDepth.replace("_", " ")} · {formatBytes(hit.size)}
              </span>
            </div>
            <div className="hit-path">{hit.relPath}</div>
            {hit.snippet && <div className="hit-snippet">{hit.snippet}</div>}
          </article>
        ))}
      </section>
    </main>
  );
}
