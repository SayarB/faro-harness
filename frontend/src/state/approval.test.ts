import { describe, expect, it } from "vitest";
import { applyApprovalEvent } from "./approval";
import type { FaroEvent } from "../lib/events";

const req = (id: string): FaroEvent => ({
  type: "approval.request", turnId: "t1", approvalId: id, kind: "command", command: "echo hi",
});
const resolved = (id: string): FaroEvent => ({ type: "approval.resolved", turnId: "t1", approvalId: id, decision: "deny" });

describe("approval state", () => {
  it("holds a request until it is resolved", () => {
    const pending = applyApprovalEvent(null, req("a1"));
    expect(pending).toMatchObject({ approvalId: "a1", kind: "command" });
    expect(applyApprovalEvent(pending, resolved("a1"))).toBeNull();
  });

  it("ignores a stale resolve for an older request", () => {
    const pending = applyApprovalEvent(null, req("a2"));
    expect(applyApprovalEvent(pending, resolved("a1"))).toBe(pending);
  });

  it("clears when the turn ends, whatever the reason", () => {
    const pending = applyApprovalEvent(null, req("a1"));
    for (const type of ["turn.done", "turn.error", "turn.cancelled"] as const) {
      const ev = { type, turnId: "t1", reply: "", error: "" } as FaroEvent;
      expect(applyApprovalEvent(pending, ev)).toBeNull();
    }
  });

  it("leaves it alone for unrelated events", () => {
    const pending = applyApprovalEvent(null, req("a1"));
    expect(applyApprovalEvent(pending, { type: "token", turnId: "t1", text: "x" })).toBe(pending);
  });
});
