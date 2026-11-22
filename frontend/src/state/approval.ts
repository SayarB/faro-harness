// Pure approval state. At most one request is pending at a time (tool calls in a turn are
// sequential), keyed by approvalId and turnId so an unrelated turn end or stale resolve never
// clears a live request. Approvals apply regardless of which conversation is open in the UI.
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
      return cur?.turnId === ev.turnId ? null : cur;
    default:
      return cur;
  }
}
