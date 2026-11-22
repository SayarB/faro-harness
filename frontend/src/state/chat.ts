// Pure chat state + reducer. All event handling for a conversation lives here (no React, no
// Wails) so it is trivially testable and identical for UI-started and CLI-started turns.
import type { ChatEvent, FaroEvent, Snapshot } from "../lib/events";
import { applyApprovalEvent, type PendingApproval } from "./approval";

export type ToolStatus = "running" | "done" | "error" | "declined";

export type Part =
  | { kind: "text"; text: string }
  | {
      kind: "tool";
      callId: string;
      name: string;
      args: string;
      status: ToolStatus;
      result?: string;
      durationMs?: number;
      /** The user said no in the approval dialog; shown as "You declined", not as a failure. */
      declined?: boolean;
    };

export type MessageStatus = "streaming" | "done" | "error" | "cancelled";

export interface Message {
  id: string;
  role: "user" | "assistant";
  parts: Part[];
  status: MessageStatus;
  turnId: string;
  error?: string;
}

export interface ChatState {
  messages: Message[];
}

export const emptyChat: ChatState = { messages: [] };

export function isBusy(chat: ChatState): boolean {
  return chat.messages.some((m) => m.role === "assistant" && m.status === "streaming");
}

function locate(messages: Message[], turnId: string): number {
  return messages.findIndex((m) => m.role === "assistant" && m.turnId === turnId);
}

function toolResultFailed(result: string): boolean {
  try {
    const v: unknown = JSON.parse(result);
    return typeof v === "object" && v !== null && "error" in v && typeof (v as { error: unknown }).error === "string";
  } catch {
    return false;
  }
}

export function applyEvent(chat: ChatState, ev: ChatEvent): ChatState {
  if (ev.type === "turn.started") {
    if (locate(chat.messages, ev.turnId) >= 0) return chat;
    return {
      messages: [
        ...chat.messages,
        {
          id: `${ev.turnId}:user`,
          role: "user",
          parts: [{ kind: "text", text: ev.text }],
          status: "done",
          turnId: ev.turnId,
        },
        {
          id: `${ev.turnId}:assistant`,
          role: "assistant",
          parts: [],
          status: "streaming",
          turnId: ev.turnId,
        },
      ],
    };
  }

  const idx = locate(chat.messages, ev.turnId);
  if (idx < 0) return chat;

  const current = chat.messages[idx];
  const next = reduceAssistant(current, ev);
  const messages = chat.messages.slice();
  messages[idx] = next;
  return { messages };
}

function reduceAssistant(m: Message, ev: Exclude<ChatEvent, { type: "turn.started" }>): Message {
  switch (ev.type) {
    case "token": {
      const parts = m.parts.slice();
      const last = parts[parts.length - 1];
      if (last?.kind === "text") parts[parts.length - 1] = { kind: "text", text: last.text + ev.text };
      else parts.push({ kind: "text", text: ev.text });
      return { ...m, parts };
    }
    case "tool_call":
      return {
        ...m,
        parts: [
          ...m.parts,
          { kind: "tool", callId: ev.toolCallId, name: ev.toolName, args: ev.toolArgs, status: "running" },
        ],
      };
    case "tool_result":
      return {
        ...m,
        parts: m.parts.map((p) =>
          p.kind === "tool" && p.callId === ev.toolCallId
            ? {
                ...p,
                status: p.declined
                  ? ("declined" as const)
                  : toolResultFailed(ev.toolResult)
                    ? ("error" as const)
                    : ("done" as const),
                result: ev.toolResult,
                durationMs: ev.durationMs,
              }
            : p,
        ),
      };
    case "approval.resolved": {
      if (ev.decision !== "deny") return m;
      // Tool calls run one at a time, so the call waiting on this approval is the last running one.
      const i = m.parts.map((p) => p.kind === "tool" && p.status === "running").lastIndexOf(true);
      if (i < 0) return m;
      const parts = m.parts.slice();
      parts[i] = { ...(parts[i] as Extract<Part, { kind: "tool" }>), declined: true };
      return { ...m, parts };
    }
    case "turn.done": {
      const hasText = m.parts.some((p) => p.kind === "text" && p.text.length > 0);
      const parts = hasText || !ev.reply ? m.parts : [...m.parts, { kind: "text" as const, text: ev.reply }];
      return { ...m, parts, status: "done" };
    }
    case "turn.error":
      return { ...m, status: "error", error: ev.error };
    case "turn.cancelled":
      return { ...m, status: "cancelled" };
    case "usage":
      return m; // not shown in the MVP UI
  }
}

export interface ReconciledSnapshot {
  chat: ChatState;
  approval: PendingApproval | null;
}

/** Replays a conversation snapshot and flushes live events buffered while OpenChat was in flight,
 *  dropping buffered events already covered by the snapshot (seq <= snapshot.lastSeq). */
export function replaySnapshot(
  snapshot: Snapshot,
  buffered: FaroEvent[],
  currentApproval: PendingApproval | null = null,
): ReconciledSnapshot {
  let chat = emptyChat;
  let approval = currentApproval;
  for (const ev of snapshot.events) {
    approval = applyApprovalEvent(approval, ev);
    if (ev.type !== "index.progress" && ev.type !== "approval.request") {
      chat = applyEvent(chat, ev);
    }
  }
  for (const ev of buffered) {
    if (ev.type === "index.progress") continue;
    if (ev.seq !== undefined && ev.seq <= snapshot.lastSeq) continue;
    approval = applyApprovalEvent(approval, ev);
    if (ev.conversationId === snapshot.conversationId && ev.type !== "approval.request") {
      chat = applyEvent(chat, ev);
    }
  }
  return { chat, approval };
}
