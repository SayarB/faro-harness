import { describe, expect, it } from "vitest";
import { applyEvent, emptyChat, isBusy, replaySnapshot, type ChatState } from "./chat";
import type { ChatEvent, FaroEvent, Snapshot } from "../lib/events";

const run = (chat: ChatState, ...evs: ChatEvent[]) => evs.reduce(applyEvent, chat);
const started = (turnId = "t1", text = "hi", conversationId = "conv_1"): ChatEvent => ({
  type: "turn.started",
  conversationId,
  turnId,
  text,
});

describe("chat reducer", () => {
  it("creates user and streaming assistant messages from turn.started", () => {
    const chat = run(emptyChat, started("t1", "hello"));
    expect(chat.messages.map((m) => [m.role, m.status, m.turnId])).toEqual([
      ["user", "done", "t1"],
      ["assistant", "streaming", "t1"],
    ]);
    expect(chat.messages[0].parts).toEqual([{ kind: "text", text: "hello" }]);
    expect(isBusy(chat)).toBe(true);
  });

  it("handles CLI-started (foreign) turns through the exact same turn.started path", () => {
    const chat = run(
      emptyChat,
      started("turn_cli", "from terminal"),
      { type: "token", conversationId: "conv_1", turnId: "turn_cli", text: "Answer" },
      { type: "turn.done", conversationId: "conv_1", turnId: "turn_cli", reply: "Answer" },
    );
    expect(chat.messages).toHaveLength(2);
    expect(chat.messages[0].parts).toEqual([{ kind: "text", text: "from terminal" }]);
    expect(chat.messages[1]).toMatchObject({ status: "done", turnId: "turn_cli" });
    expect(chat.messages[1].parts).toEqual([{ kind: "text", text: "Answer" }]);
    expect(isBusy(chat)).toBe(false);
  });

  it("ignores duplicate turn.started for an existing turnId", () => {
    const once = run(emptyChat, started("t1", "hi"));
    const twice = applyEvent(once, started("t1", "hi"));
    expect(twice).toBe(once);
  });

  it("merges streamed tokens into one text part", () => {
    const chat = run(
      emptyChat,
      started("t1"),
      { type: "token", turnId: "t1", text: "Hel" },
      { type: "token", turnId: "t1", text: "lo" },
    );
    const a = chat.messages[1];
    expect(a.turnId).toBe("t1");
    expect(a.parts).toEqual([{ kind: "text", text: "Hello" }]);
  });

  it("pairs tool results with their calls and flags tool errors", () => {
    const chat = run(
      emptyChat,
      started("t1"),
      { type: "tool_call", turnId: "t1", toolCallId: "c1", toolName: "search_files", toolArgs: '{"query":"x"}' },
      { type: "tool_call", turnId: "t1", toolCallId: "c2", toolName: "read_file", toolArgs: "{}" },
      { type: "tool_result", turnId: "t1", toolCallId: "c1", toolName: "search_files", toolResult: '{"hits":[]}', durationMs: 12 },
      { type: "tool_result", turnId: "t1", toolCallId: "c2", toolName: "read_file", toolResult: '{"error":"not found"}' },
    );
    const parts = chat.messages[1].parts;
    expect(parts[0]).toMatchObject({ kind: "tool", callId: "c1", status: "done", durationMs: 12 });
    expect(parts[1]).toMatchObject({ kind: "tool", callId: "c2", status: "error" });
  });

  it("keeps text and tool parts in arrival order", () => {
    const chat = run(
      emptyChat,
      started("t1"),
      { type: "token", turnId: "t1", text: "Looking…" },
      { type: "tool_call", turnId: "t1", toolCallId: "c1", toolName: "list_folder", toolArgs: "{}" },
      { type: "token", turnId: "t1", text: "Done." },
    );
    expect(chat.messages[1].parts.map((p) => p.kind)).toEqual(["text", "tool", "text"]);
  });

  it("completes on turn.done and uses the reply when nothing was streamed", () => {
    const chat = run(emptyChat, started("t1"), { type: "turn.done", turnId: "t1", reply: "Final answer" });
    const a = chat.messages[1];
    expect(a.status).toBe("done");
    expect(a.parts).toEqual([{ kind: "text", text: "Final answer" }]);
    expect(isBusy(chat)).toBe(false);
  });

  it("does not duplicate text when the reply was already streamed", () => {
    const chat = run(
      emptyChat,
      started("t1"),
      { type: "token", turnId: "t1", text: "Hi" },
      { type: "turn.done", turnId: "t1", reply: "Hi" },
    );
    expect(chat.messages[1].parts).toEqual([{ kind: "text", text: "Hi" }]);
  });

  it("records errors and cancellations", () => {
    const err = run(emptyChat, started("t1"), { type: "turn.error", turnId: "t1", error: "boom" });
    expect(err.messages[1]).toMatchObject({ status: "error", error: "boom" });
    const cancelled = run(emptyChat, started("t2"), { type: "turn.cancelled", turnId: "t2" });
    expect(cancelled.messages[1].status).toBe("cancelled");
  });

  it("ignores events for unknown turns", () => {
    const done = run(emptyChat, started("t1"), { type: "turn.done", turnId: "t1", reply: "ok" });
    const after = applyEvent(done, { type: "token", turnId: "stale", text: "x" });
    expect(after).toBe(done);
  });
});

describe("approval declines", () => {
  const call: ChatEvent = { type: "tool_call", turnId: "t1", toolCallId: "c1", toolName: "deliver_file", toolArgs: "{}" };
  const denied: ChatEvent = { type: "approval.resolved", turnId: "t1", approvalId: "a1", decision: "deny" };
  const refused: ChatEvent = {
    type: "tool_result", turnId: "t1", toolCallId: "c1", toolName: "deliver_file",
    toolResult: '{"error":"file delivery denied by user"}',
  };

  it("shows a user denial as declined, not as an error", () => {
    const chat = run(emptyChat, started("t1"), call, denied, refused);
    expect(chat.messages[1].parts[0]).toMatchObject({ kind: "tool", status: "declined" });
  });

  it("keeps a real failure as an error when the user allowed it", () => {
    const allowed: ChatEvent = { ...denied, decision: "allow" };
    const chat = run(emptyChat, started("t1"), call, allowed, refused);
    expect(chat.messages[1].parts[0]).toMatchObject({ status: "error" });
  });

  it("does not mark a cancelled approval as declined", () => {
    const cancelled: ChatEvent = { ...denied, decision: "cancelled" };
    const chat = run(emptyChat, started("t1"), call, cancelled, { type: "turn.cancelled", turnId: "t1" });
    expect(chat.messages[1].parts[0]).toMatchObject({ status: "running" });
    expect((chat.messages[1].parts[0] as { declined?: boolean }).declined).toBeUndefined();
  });
});

describe("replaySnapshot", () => {
  it("replays history plus an in-flight turn and deduplicates buffered events by seq", () => {
    const snapshot: Snapshot = {
      conversationId: "conv_1",
      botSlug: "faro",
      busy: true,
      lastSeq: 10,
      events: [
        // Synthetic history turn (seq 0)
        { type: "turn.started", conversationId: "conv_1", turnId: "hist-1", text: "q1" },
        { type: "token", conversationId: "conv_1", turnId: "hist-1", text: "a1" },
        { type: "turn.done", conversationId: "conv_1", turnId: "hist-1", reply: "a1" },
        // Running turn already up to seq 10
        { type: "turn.started", conversationId: "conv_1", seq: 8, turnId: "turn_2", text: "q2" },
        { type: "token", conversationId: "conv_1", seq: 10, turnId: "turn_2", text: "Hel" },
        {
          type: "approval.request",
          conversationId: "conv_1",
          seq: 10,
          turnId: "turn_2",
          approvalId: "appr_1",
          kind: "command",
          command: "echo hi",
        },
      ],
    };

    const buffered: FaroEvent[] = [
      // Duplicate covered by snapshot (seq <= 10) -> must be dropped
      { type: "token", conversationId: "conv_1", seq: 10, turnId: "turn_2", text: "Hel" },
      // Unrelated conversation -> ignored for chat
      { type: "turn.started", conversationId: "conv_other", seq: 11, turnId: "turn_other", text: "nope" },
      // New live events after snapshot (seq > 10) -> must be applied
      { type: "approval.resolved", conversationId: "conv_1", seq: 12, turnId: "turn_2", approvalId: "appr_1", decision: "allow" },
      { type: "token", conversationId: "conv_1", seq: 13, turnId: "turn_2", text: "lo!" },
      { type: "turn.done", conversationId: "conv_1", seq: 14, turnId: "turn_2", reply: "Hello!" },
    ];

    const res = replaySnapshot(snapshot, buffered, null);
    expect(res.approval).toBeNull();
    expect(res.chat.messages).toHaveLength(4);
    expect(res.chat.messages[0].parts).toEqual([{ kind: "text", text: "q1" }]);
    expect(res.chat.messages[1].parts).toEqual([{ kind: "text", text: "a1" }]);
    expect(res.chat.messages[2].parts).toEqual([{ kind: "text", text: "q2" }]);
    expect(res.chat.messages[3].parts).toEqual([{ kind: "text", text: "Hello!" }]);
    expect(res.chat.messages[3].status).toBe("done");
    expect(isBusy(res.chat)).toBe(false);
  });

  it("keeps a pending approval from the snapshot when not yet resolved", () => {
    const snapshot: Snapshot = {
      conversationId: "conv_1",
      botSlug: "faro",
      busy: true,
      lastSeq: 5,
      events: [
        { type: "turn.started", conversationId: "conv_1", seq: 4, turnId: "t1", text: "run it" },
        {
          type: "approval.request",
          conversationId: "conv_1",
          seq: 5,
          turnId: "t1",
          approvalId: "appr_live",
          kind: "command",
          command: "ls",
        },
      ],
    };
    const res = replaySnapshot(snapshot, [], null);
    expect(res.approval).toMatchObject({ approvalId: "appr_live", command: "ls" });
    expect(isBusy(res.chat)).toBe(true);
  });
});
