import { useState } from "react";
import { useBusy, useStore } from "../../state/store";
import { Button } from "../../ui/Button";
import { cn } from "../../ui/cn";
import { NewBotDialog } from "./NewBotDialog";

export function BotList() {
  const bots = useStore((s) => s.bots);
  const botSlug = useStore((s) => s.botSlug);
  const selectBot = useStore((s) => s.selectBot);
  const busy = useBusy();
  const [creating, setCreating] = useState(false);

  const item = (slug: string, label: string) => (
    <li key={slug}>
      <button
        disabled={busy}
        onClick={() => selectBot(slug)}
        className={cn(
          "w-full truncate rounded-ui px-2 py-1.5 text-left text-sm disabled:opacity-60",
          botSlug === slug ? "bg-surface-2 font-medium" : "hover:bg-surface-2",
        )}
      >
        {label}
      </button>
    </li>
  );

  return (
    <section className="space-y-2">
      <div className="flex items-center justify-between">
        <h2 className="text-xs font-semibold tracking-wide text-muted uppercase">Assistant</h2>
        <Button variant="ghost" className="px-2 py-0.5 text-xs" onClick={() => setCreating(true)}>
          + New bot
        </Button>
      </div>
      <ul className="space-y-1">
        {bots.map((b) => item(b.slug, b.name))}
      </ul>
      <NewBotDialog open={creating} onOpenChange={setCreating} />
    </section>
  );
}
