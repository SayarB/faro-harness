import { useState } from "react";
import { useStore } from "../../state/store";
import { Button } from "../../ui/Button";
import { Dialog } from "../../ui/Dialog";
import { Input, Textarea } from "../../ui/Field";

export function NewBotDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const createBot = useStore((s) => s.createBot);
  const [name, setName] = useState("");
  const [instructions, setInstructions] = useState("");
  const [saving, setSaving] = useState(false);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setSaving(true);
    const ok = await createBot(name, instructions);
    setSaving(false);
    if (ok) {
      setName("");
      setInstructions("");
      onOpenChange(false);
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title="New bot"
      description="A bot is an assistant with its own memory and instructions."
    >
      <form onSubmit={submit} className="space-y-3">
        <Input
          autoFocus
          placeholder="Name, e.g. Ledger"
          value={name}
          onChange={(e) => setName(e.target.value)}
          aria-label="Bot name"
        />
        <Textarea
          rows={4}
          placeholder="What should this bot do? (optional)"
          value={instructions}
          onChange={(e) => setInstructions(e.target.value)}
          aria-label="Bot instructions"
        />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={!name.trim() || saving}>
            Create bot
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
