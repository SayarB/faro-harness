// Friendly wording for tool calls. Data-only: adding a tool here (or not) never breaks the UI,
// unknown tools fall back to their raw name.
interface ToolLabel {
  running: string;
  done: string;
  /** Wording when the user declined; defaults to `done` (the card also says "You declined"). */
  declined?: string;
}

const LABELS: Record<string, ToolLabel> = {
  search_files: { running: "Searching your files", done: "Searched your files" },
  list_folder: { running: "Looking in a folder", done: "Looked in a folder" },
  read_file: { running: "Reading a file", done: "Read a file" },
  deliver_file: { running: "Saving a file", done: "Saved a file", declined: "Save a file" },
  bot_list: { running: "Checking its notes", done: "Checked its notes" },
  bot_read: { running: "Reading its notes", done: "Read its notes" },
  bot_write: { running: "Writing a note", done: "Wrote a note" },
  bot_delete: { running: "Removing a note", done: "Removed a note" },
  bot_run_command: { running: "Running a command", done: "Ran a command", declined: "Run a command" },
  conversation_history: { running: "Checking earlier messages", done: "Checked earlier messages" },
};

export function toolLabel(name: string, status: "running" | "done" | "error" | "declined"): string {
  const l = LABELS[name];
  if (!l) return name;
  if (status === "declined") return l.declined ?? l.done;
  return status === "running" ? l.running : l.done;
}

const SUMMARY_KEYS = ["query", "rel_path", "dest_rel_path", "folder", "path", "command"];

/** One short detail for the card header, taken from the call's arguments. */
export function toolSummary(args: string): string {
  try {
    const v: unknown = JSON.parse(args);
    if (typeof v === "object" && v !== null) {
      const o = v as Record<string, unknown>;
      for (const k of SUMMARY_KEYS) {
        if (typeof o[k] === "string" && o[k]) return o[k] as string;
      }
    }
  } catch {
    /* fall through */
  }
  return "";
}

export function prettyJson(s: string): string {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}
