// Pure chat state + reducer. All event handling for a conversation lives here (no React, no
// Wails) so it is trivially testable and the one place to extend when new event types arrive.
import type { ChatEvent } from "../lib/events";

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
  /** Set on assistant messages once the first event of their turn arrives. */
  turnId?: string;
  error?: string;
}

export interface ChatState {
  messages: Message[];
}

export const emptyChat: ChatState = { messages: [] };

export function isBusy(chat: ChatState): boolean {
  return chat.messages.some((m) => m.role === "assistant" && m.status === "streaming");
}

/** Adds the user's message and an empty streaming assistant message awaiting its turn id. */
export function beginTurn(
  chat: ChatState,
  text: string,
  ids: { user: string; assistant: string },
): ChatState {
  return {
    messages: [
      ...chat.messages,
      { id: ids.user, role: "user", parts: [{ kind: "text", text }], status: "done" },
      { id: ids.assistant, role: "assistant", parts: [], status: "streaming" },
    ],
  };
}

/** Marks the not-yet-started assistant message as failed (e.g. Send was rejected). */
export function failPending(chat: ChatState, error: string): ChatState {
  return {
    messages: chat.messages.map((m) =>
      m.role === "assistant" && m.status === "streaming" && !m.turnId
        ? { ...m, status: "error" as const, error }
        : m,
    ),
  };
}

// Events can arrive before the Send() promise resolves, so the first event of a turn claims the
// newest assistant message that has no turn id yet.
function locate(messages: Message[], turnId: string): number {
  const own = messages.findIndex((m) => m.role === "assistant" && m.turnId === turnId);
  if (own >= 0) return own;
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (m.role === "assistant" && m.status === "streaming" && !m.turnId) return i;
  }
  return -1;
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
  const idx = locate(chat.messages, ev.turnId);
  if (idx < 0) return chat;

  const current = chat.messages[idx];
  const next = reduceAssistant({ ...current, turnId: ev.turnId }, ev);
  const messages = chat.messages.slice();
  messages[idx] = next;
  return { messages };
}

function reduceAssistant(m: Message, ev: ChatEvent): Message {
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
