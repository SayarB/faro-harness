import { useState } from "react";
import { useStore } from "../../state/store";
import { Button } from "../../ui/Button";
import { Input } from "../../ui/Field";

export function ApiKeyScreen() {
  const saveKey = useStore((s) => s.saveKey);
  const [key, setKey] = useState("");
  const [saving, setSaving] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setSaving(true);
    await saveKey(key);
    setSaving(false);
  }

  return (
    <div className="flex h-full items-center justify-center p-6">
      <form onSubmit={submit} className="w-full max-w-md space-y-4 rounded-ui border border-line bg-surface p-6">
        <div>
          <h1 className="text-xl font-semibold">Welcome to Faro</h1>
          <p className="mt-1 text-sm text-muted">
            Faro answers questions about the files in folders you choose. To get started, paste your Vercel AI
            Gateway API key.
          </p>
        </div>
        <Input
          type="password"
          autoFocus
          placeholder="API key"
          value={key}
          onChange={(e) => setKey(e.target.value)}
          aria-label="API key"
        />
        <ul className="list-disc space-y-1 pl-5 text-xs text-muted">
          <li>The key is stored on this computer in a file only your user account can read. It is not encrypted.</li>
          <li>
            Pieces of your files that Faro reads to answer a question are sent to the AI provider through that
            gateway. Your folder index itself stays on this computer.
          </li>
        </ul>
        <Button type="submit" variant="primary" disabled={!key.trim() || saving} className="w-full">
          Continue
        </Button>
      </form>
    </div>
  );
}
