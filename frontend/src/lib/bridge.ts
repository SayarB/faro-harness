// The only module that talks to Wails. Everything else imports from here, so swapping the
// transport (tests, a browser dev mock, a future remote backend) touches one file.
import * as App from "../../wailsjs/go/main/App";
import { EventsOn } from "../../wailsjs/runtime/runtime";
import type { desktop } from "../../wailsjs/go/models";
import type { ApprovalDecision, FaroEvent } from "./events";

export type AppState = desktop.State;
export type Workspace = desktop.WorkspaceInfo;
export type Bot = desktop.BotInfo;
export type ChatInfo = desktop.ChatInfo;

// Must match eventChannel in app.go.
const EVENT_CHANNEL = "faro:event";

export const bridge = {
  getState: (): Promise<AppState> => App.GetState(),
  saveApiKey: (key: string): Promise<void> => App.SaveAPIKey(key),
  /** Opens the native folder dialog; resolves to null if the user cancelled. */
  pickFolder: async (): Promise<Workspace | null> => (await App.PickFolder()) ?? null,
  rescan: (workspaceId: number): Promise<void> => App.Rescan(workspaceId),
  createBot: (name: string, instructions: string): Promise<Bot> => App.CreateBot(name, instructions),
  /** Starts a fresh conversation with the given bot. Every chat has a bot. */
  newChat: (botSlug: string): Promise<ChatInfo> => App.NewChat(botSlug),
  /** Starts a turn and resolves with its id; progress arrives via onEvent. */
  send: (text: string): Promise<string> => App.Send(text),
  cancel: (): Promise<void> => App.Cancel(),
  /** Answers the pending approval dialog. Human-only: never routed through the model. */
  respondApproval: (id: string, decision: ApprovalDecision): Promise<void> => App.RespondApproval(id, decision),
  /** Subscribes to the single event stream. Returns an unsubscribe function. */
  onEvent: (cb: (ev: FaroEvent) => void): (() => void) =>
    EventsOn(EVENT_CHANNEL, (ev: FaroEvent) => cb(ev)),
};

/** Wails rejects with plain strings; normalise anything thrown into a message. */
export function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return typeof err === "string" ? err : "Something went wrong.";
}
