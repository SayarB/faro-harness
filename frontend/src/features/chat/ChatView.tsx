import { useEffect, useRef, useState } from "react";
import { useStore } from "../../state/store";
import { Button } from "../../ui/Button";
import { Composer } from "./Composer";
import { MessageView } from "./MessageView";

export function ChatView() {
  const messages = useStore((s) => s.chat.messages);
  const bots = useStore((s) => s.bots);
  const botSlug = useStore((s) => s.botSlug);
  const conversationId = useStore((s) => s.conversationId);
  const hasFolder = useStore((s) => s.workspaces.length > 0);
  const addFolder = useStore((s) => s.addFolder);
  const [copied, setCopied] = useState(false);

  const title = bots.find((b) => b.slug === botSlug)?.name ?? "Faro";

  useEffect(() => {
    setCopied(false);
  }, [conversationId]);

  const copyId = async () => {
    if (!conversationId) return;
    try {
      await navigator.clipboard.writeText(conversationId);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // clipboard unavailable
    }
  };

  // Keep the newest content in view unless the user scrolled up.
  const scroller = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  useEffect(() => {
    const el = scroller.current;
    if (el && stick.current) el.scrollTop = el.scrollHeight;
  }, [messages]);

  return (
    <>
      <header className="flex items-center justify-between gap-3 border-b border-line px-4 py-2.5">
        <div className="flex min-w-0 items-center gap-2">
          <h1 className="truncate text-sm font-semibold">{title}</h1>
          {conversationId ? (
            <div className="flex min-w-0 items-center gap-1 rounded-ui bg-surface-2 px-2 py-0.5 text-xs text-muted">
              <code className="truncate font-mono">{conversationId}</code>
              <button
                type="button"
                onClick={() => void copyId()}
                title="Copy conversation ID for faro-cli attach"
                className="shrink-0 text-fg/80 hover:text-fg"
              >
                {copied ? "Copied" : "Copy"}
              </button>
            </div>
          ) : null}
        </div>
      </header>
      <div
        ref={scroller}
        className="flex-1 overflow-y-auto"
        onScroll={(e) => {
          const el = e.currentTarget;
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
        }}
      >
        <div className="mx-auto max-w-3xl space-y-5 px-4 py-6">
          {messages.length === 0 ? (
            <div className="py-16 text-center text-sm text-muted">
              <div className="space-y-3">
                <p>
                  {hasFolder
                    ? `Ask ${title} about the files in your folders, or to make a Word, Excel or PDF document.`
                    : `Ask ${title} to make a document, or add a folder so it can read your files.`}
                </p>
                {!hasFolder && (
                  <Button variant="secondary" onClick={() => void addFolder()}>
                    Add a folder
                  </Button>
                )}
              </div>
            </div>
          ) : (
            messages.map((m) => <MessageView key={m.id} message={m} />)
          )}
        </div>
      </div>
      <Composer />
    </>
  );
}
