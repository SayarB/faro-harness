import Markdown from "react-markdown";
import type { Message } from "../../state/chat";
import { Spinner } from "../../ui/Spinner";
import { cn } from "../../ui/cn";
import { ToolCallCard } from "./ToolCallCard";

// Links are rendered as plain text on purpose: replies can echo untrusted file contents, and the
// desktop app must not navigate anywhere because of text a model wrote.
const markdownComponents = {
  a: ({ children }: { children?: React.ReactNode }) => <span className="underline">{children}</span>,
};

export function MessageView({ message }: { message: Message }) {
  if (message.role === "user") {
    return (
      <div className="flex justify-end">
        <div className="max-w-[80%] rounded-ui bg-accent px-3.5 py-2 text-sm whitespace-pre-wrap text-accent-fg">
          {message.parts.map((p) => (p.kind === "text" ? p.text : "")).join("")}
        </div>
      </div>
    );
  }

  const waiting = message.status === "streaming" && message.parts.length === 0;
  return (
    <div className="space-y-2">
      {message.parts.map((p, i) =>
        p.kind === "tool" ? (
          <ToolCallCard key={p.callId} part={p} />
        ) : (
          <div key={i} className="prose-faro text-sm leading-relaxed">
            <Markdown components={markdownComponents}>{p.text}</Markdown>
          </div>
        ),
      )}
      {waiting ? (
        <div className="flex items-center gap-2 text-sm text-muted">
          <Spinner /> Thinking…
        </div>
      ) : null}
      {message.status === "cancelled" ? <p className="text-xs text-muted">Stopped.</p> : null}
      {message.status === "error" ? (
        <p className={cn("rounded-ui border border-danger/40 px-3 py-2 text-sm text-danger")}>
          {message.error || "Something went wrong."}
        </p>
      ) : null}
    </div>
  );
}
