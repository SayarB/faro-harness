import { describe, expect, it } from "vitest";
import { applyEvent, beginTurn, emptyChat, failPending, isBusy, type ChatState } from "./chat";
import type { ChatEvent } from "../lib/events";

const start = (): ChatState => beginTurn(emptyChat, "hi", { user: "u1", assistant: "a1" });
const run = (chat: ChatState, ...evs: ChatEvent[]) => evs.reduce(applyEvent, chat);

describe("chat reducer", () => {
  it("adds a user message and a streaming assistant placeholder", () => {
    const chat = start();
    expect(chat.messages.map((m) => [m.role, m.status])).toEqual([
      ["user", "done"],
      ["assistant", "streaming"],
    ]);
    expect(isBusy(chat)).toBe(true);
  });

  it("merges streamed tokens into one text part and claims the turn id", () => {
    const chat = run(
      start(),
      { type: "token", turnId: "t1", text: "Hel" },
      { type: "token", turnId: "t1", text: "lo" },
    );
    const a = chat.messages[1];
    expect(a.turnId).toBe("t1");
    expect(a.parts).toEqual([{ kind: "text", text: "Hello" }]);
  });

  it("pairs tool results with their calls and flags tool errors", () => {
    const chat = run(
      start(),
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
      start(),
      { type: "token", turnId: "t1", text: "Looking…" },
      { type: "tool_call", turnId: "t1", toolCallId: "c1", toolName: "list_folder", toolArgs: "{}" },
      { type: "token", turnId: "t1", text: "Done." },
    );
    expect(chat.messages[1].parts.map((p) => p.kind)).toEqual(["text", "tool", "text"]);
  });

  it("completes on turn.done and uses the reply when nothing was streamed", () => {
    const chat = run(start(), { type: "turn.done", turnId: "t1", reply: "Final answer" });
    const a = chat.messages[1];
    expect(a.status).toBe("done");
    expect(a.parts).toEqual([{ kind: "text", text: "Final answer" }]);
    expect(isBusy(chat)).toBe(false);
  });

  it("does not duplicate text when the reply was already streamed", () => {
    const chat = run(
      start(),
      { type: "token", turnId: "t1", text: "Hi" },
      { type: "turn.done", turnId: "t1", reply: "Hi" },
    );
    expect(chat.messages[1].parts).toEqual([{ kind: "text", text: "Hi" }]);
  });

  it("records errors and cancellations", () => {
    const err = run(start(), { type: "turn.error", turnId: "t1", error: "boom" });
    expect(err.messages[1]).toMatchObject({ status: "error", error: "boom" });
    const cancelled = run(start(), { type: "turn.cancelled", turnId: "t2" });
    expect(cancelled.messages[1].status).toBe("cancelled");
  });

  it("ignores events for unknown turns once nothing is pending", () => {
    const done = run(start(), { type: "turn.done", turnId: "t1", reply: "ok" });
    const after = applyEvent(done, { type: "token", turnId: "stale", text: "x" });
    expect(after).toBe(done);
  });

  it("fails only the unclaimed pending message when Send is rejected", () => {
    const chat = failPending(start(), "no folder");
    expect(chat.messages[1]).toMatchObject({ status: "error", error: "no folder" });
    expect(chat.messages[0].status).toBe("done");
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
    const chat = run(start(), call, denied, refused);
    expect(chat.messages[1].parts[0]).toMatchObject({ kind: "tool", status: "declined" });
  });

  it("keeps a real failure as an error when the user allowed it", () => {
    const allowed: ChatEvent = { ...denied, decision: "allow" };
    const chat = run(start(), call, allowed, refused);
    expect(chat.messages[1].parts[0]).toMatchObject({ status: "error" });
  });

  it("does not mark a cancelled approval as declined", () => {
    const cancelled: ChatEvent = { ...denied, decision: "cancelled" };
    const chat = run(start(), call, cancelled, { type: "turn.cancelled", turnId: "t1" });
    expect(chat.messages[1].parts[0]).toMatchObject({ status: "running" });
    expect((chat.messages[1].parts[0] as { declined?: boolean }).declined).toBeUndefined();
  });
});
