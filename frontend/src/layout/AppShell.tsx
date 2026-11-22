import type { ReactNode } from "react";

/**
 * Sidebar + main layout. `aside` is an optional right-hand panel slot (sources, file preview, …)
 * that nothing fills yet; it exists so a richer layout does not need to restructure the app.
 */
export function AppShell({
  sidebar,
  children,
  aside,
}: {
  sidebar: ReactNode;
  children: ReactNode;
  aside?: ReactNode;
}) {
  return (
    <div className="flex h-full">
      <aside className="flex w-64 shrink-0 flex-col border-r border-line bg-surface">{sidebar}</aside>
      <main className="flex min-w-0 flex-1 flex-col">{children}</main>
      {aside ? <aside className="w-80 shrink-0 border-l border-line bg-surface">{aside}</aside> : null}
    </div>
  );
}
