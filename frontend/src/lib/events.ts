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

export type ApprovalKind = "command" | "file_create" | "file_overwrite";

/** Every string here is escaped by Go (approval.EscapeControl) and comes from the Go request,
 *  not from model prose. Omitted booleans/numbers mean false/0: an absent `sandboxed` is
 *  unsandboxed. */
interface ApprovalRequestFields {
  approvalId: string;
  kind: ApprovalKind;
  command?: string;
  cwd?: string;
  workspaceName?: string;
  destRelPath?: string;
  sizeBytes?: number;
  existingSizeBytes?: number;
  preview?: string;
  sandboxed?: boolean;
  botName?: string;
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
  | (TurnScoped & ApprovalRequestFields & { type: "approval.request" })
  | (TurnScoped & {
      type: "approval.resolved";
      approvalId: string;
      /** "cancelled" means the turn was stopped before the user answered. */
      decision?: "allow" | "allow_session" | "deny" | "cancelled";
    })
  | {
      type: "index.progress";
      workspaceId: number;
      indexed?: number;
      skipped?: number;
      indexErrors?: number;
      done?: boolean;
      error?: string;
    };

export type ChatEvent = Exclude<FaroEvent, { type: "index.progress" | "approval.request" }>;
export type IndexEvent = Extract<FaroEvent, { type: "index.progress" }>;
export type ApprovalRequestEvent = Extract<FaroEvent, { type: "approval.request" }>;
export type ApprovalDecision = "allow" | "allow_session" | "deny";
