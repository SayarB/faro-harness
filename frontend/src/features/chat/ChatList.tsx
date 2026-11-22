import { useStore } from "../../state/store";
import { cn } from "../../ui/cn";

export function ChatList() {
  const chats = useStore((s) => s.chats);
  const conversationId = useStore((s) => s.conversationId);
  const openChat = useStore((s) => s.openChat);

  return (
    <section className="space-y-2">
      <h2 className="text-xs font-semibold tracking-wide text-muted uppercase">Recent chats</h2>
      {chats.length === 0 ? (
        <p className="px-2 text-xs text-muted">No chats yet.</p>
      ) : (
        <ul className="space-y-1">
          {chats.map((ch) => (
            <li key={ch.id}>
              <button
                onClick={() => void openChat(ch.id)}
                title={`${ch.botName} · ${ch.id}`}
                className={cn(
                  "w-full rounded-ui px-2 py-1.5 text-left text-sm",
                  conversationId === ch.id ? "bg-surface-2 font-medium" : "hover:bg-surface-2",
                )}
              >
                <div className="truncate">{ch.preview || "New chat"}</div>
                <div className="truncate text-xs text-muted">{ch.botName}</div>
              </button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
