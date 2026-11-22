// Pure approval state. At most one request is pending at a time (tool calls in a turn are
// sequential), but it is keyed by id so a stale resolve never clears a newer request.
import type { ApprovalRequestEvent, FaroEvent } from "../lib/events";

export type PendingApproval = ApprovalRequestEvent;

export function applyApprovalEvent(cur: PendingApproval | null, ev: FaroEvent): PendingApproval | null {
  switch (ev.type) {
    case "approval.request":
      return ev;
    case "approval.resolved":
      return cur?.approvalId === ev.approvalId ? null : cur;
    case "turn.done":
    case "turn.error":
    case "turn.cancelled":
      return null; // a finished turn can never have a live approval
    default:
      return cur;
  }
}
