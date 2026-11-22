// Mirrors internal/desktop/events.go (Event). Wails does not generate types for event payloads,
// so this union is hand-written; keep it in sync with the Go struct.

export interface Usage {
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  cached_tokens?: number;
}

interface TurnScoped {
  turnId: string;
}

export type FaroEvent =
  | (TurnScoped & { type: "token"; text: string })
  | (TurnScoped & {
      type: "tool_call";
      toolCallId: string;
      toolName: string;
      toolArgs: string;
    })
  | (TurnScoped & {
      type: "tool_result";
      toolCallId: string;
      toolName: string;
      toolResult: string;
      durationMs?: number;
    })
  | (TurnScoped & { type: "usage"; usage: Usage })
  | (TurnScoped & { type: "turn.done"; reply: string; usage?: Usage })
  | (TurnScoped & { type: "turn.error"; error: string })
  | (TurnScoped & { type: "turn.cancelled" })
  | {
      type: "index.progress";
      workspaceId: number;
      indexed?: number;
      skipped?: number;
      indexErrors?: number;
      done?: boolean;
      error?: string;
    };

export type ChatEvent = Exclude<FaroEvent, { type: "index.progress" }>;
export type IndexEvent = Extract<FaroEvent, { type: "index.progress" }>;
