import { useState } from "react";
import { useBusy, useStore } from "../../state/store";
import { Button } from "../../ui/Button";
import { Textarea } from "../../ui/Field";

export function Composer() {
  const send = useStore((s) => s.send);
  const cancel = useStore((s) => s.cancel);
  const busy = useBusy();
  const ready = useStore((s) => s.chatReady);
  const [text, setText] = useState("");

  function submit() {
    if (busy || !ready || !text.trim()) return;
    void send(text);
    setText("");
  }

  return (
    <div className="border-t border-line bg-bg p-3">
      <div className="mx-auto flex max-w-3xl items-end gap-2">
        <Textarea
          rows={2}
          value={text}
          placeholder="Ask about your files, or ask for a document…"
          aria-label="Message"
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            // Don't send while an IME composition is in progress.
            if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
              e.preventDefault();
              submit();
            }
          }}
        />
        {busy ? (
          <Button variant="secondary" onClick={() => void cancel()}>
            Stop
          </Button>
        ) : (
          <Button variant="primary" onClick={submit} disabled={!ready || !text.trim()}>
            Send
          </Button>
        )}
      </div>
    </div>
  );
}
