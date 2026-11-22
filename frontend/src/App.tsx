import { useEffect } from "react";
import { ApiKeyScreen } from "./features/onboarding/ApiKeyScreen";
import { BotList } from "./features/bots/BotList";
import { ChatView } from "./features/chat/ChatView";
import { FolderList } from "./features/workspaces/FolderList";
import { AppShell } from "./layout/AppShell";
import { useStore } from "./state/store";

export function App() {
  const view = useStore((s) => s.view);
  const init = useStore((s) => s.init);
  const notice = useStore((s) => s.notice);
  const dismissNotice = useStore((s) => s.dismissNotice);

  useEffect(() => {
    void init();
  }, [init]);

  return (
    <>
      {view === "loading" ? null : view === "onboarding" ? (
        <ApiKeyScreen />
      ) : (
        <AppShell
          sidebar={
            <>
              <div className="px-4 py-3 text-base font-semibold">Faro</div>
              <div className="flex-1 space-y-6 overflow-y-auto px-3 pb-4">
                <BotList />
                <FolderList />
              </div>
            </>
          }
        >
          <ChatView />
        </AppShell>
      )}
      {notice ? (
        <div
          role="alert"
          className="fixed right-4 bottom-4 flex max-w-sm items-start gap-3 rounded-ui border border-danger/40 bg-surface px-3 py-2 text-sm text-danger shadow-lg"
        >
          <span className="flex-1">{notice}</span>
          <button onClick={dismissNotice} aria-label="Dismiss" className="text-muted hover:text-fg">
            ✕
          </button>
        </div>
      ) : null}
    </>
  );
}
