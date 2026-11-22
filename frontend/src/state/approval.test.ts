import { describe, expect, it } from "vitest";
import { applyApprovalEvent } from "./approval";
import type { FaroEvent } from "../lib/events";

const req = (id: string, turnId = "t1", conversationId = "conv_1"): FaroEvent => ({
  type: "approval.request",
  conversationId,
  turnId,
  approvalId: id,
  kind: "command",
  command: "echo hi",
});
const resolved = (id: string, turnId = "t1"): FaroEvent => ({
  type: "approval.resolved",
  conversationId: "conv_1",
  turnId,
  approvalId: id,
  decision: "deny",
});

describe("approval state", () => {
  it("holds a request until it is resolved, regardless of conversation", () => {
    const pending = applyApprovalEvent(null, req("a1", "t1", "conv_other"));
    expect(pending).toMatchObject({ approvalId: "a1", conversationId: "conv_other", kind: "command" });
    expect(applyApprovalEvent(pending, resolved("a1"))).toBeNull();
  });

  it("ignores a stale resolve for an older request", () => {
    const pending = applyApprovalEvent(null, req("a2"));
    expect(applyApprovalEvent(pending, resolved("a1"))).toBe(pending);
  });

  it("clears when the SAME turn ends, whatever the reason", () => {
    const pending = applyApprovalEvent(null, req("a1", "t1"));
    for (const type of ["turn.done", "turn.error", "turn.cancelled"] as const) {
      const ev = { type, turnId: "t1", reply: "", error: "" } as FaroEvent;
      expect(applyApprovalEvent(pending, ev)).toBeNull();
    }
  });

  it("does NOT clear when a different turn ends (e.g. replayed history turn)", () => {
    const pending = applyApprovalEvent(null, req("a1", "t_live"));
    for (const type of ["turn.done", "turn.error", "turn.cancelled"] as const) {
      const ev = { type, turnId: "hist-1", reply: "", error: "" } as FaroEvent;
      expect(applyApprovalEvent(pending, ev)).toBe(pending);
    }
  });

  it("leaves it alone for unrelated events", () => {
    const pending = applyApprovalEvent(null, req("a1"));
    expect(applyApprovalEvent(pending, { type: "token", turnId: "t1", text: "x" })).toBe(pending);
  });
});
