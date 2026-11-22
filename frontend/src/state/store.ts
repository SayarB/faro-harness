// App state (Zustand). Chat/event logic lives in ./chat.ts; this file wires it to the bridge.
import { create } from "zustand";
import { bridge, errorMessage, type Bot, type ChatSummary, type Workspace } from "../lib/bridge";
import type { ApprovalDecision, FaroEvent } from "../lib/events";
import { applyApprovalEvent, type PendingApproval } from "./approval";
import { applyEvent, emptyChat, isBusy, replaySnapshot, type ChatState } from "./chat";

export type View = "loading" | "onboarding" | "chat";

export interface IndexProgress {
  indexed: number;
  skipped: number;
  errors: number;
  done: boolean;
  error?: string;
}

interface Store {
  view: View;
  workspaces: Workspace[];
  bots: Bot[];
  chats: ChatSummary[];
  /** Selected bot slug. Empty only until the first state load picks the default bot. */
  botSlug: string;
  /** Currently open conversation id, or null for a fresh unsent chat. */
  conversationId: string | null;
  index: Record<number, IndexProgress>;
  chat: ChatState;
  /** The approval dialog's request, if one is waiting for the user (from any conversation). */
  approval: PendingApproval | null;
  /** True once Go has a session for the current conversation. */
  chatReady: boolean;
  notice: string | null;

  init: () => Promise<void>;
  refreshChats: () => Promise<void>;
  saveKey: (key: string) => Promise<boolean>;
  addFolder: () => Promise<void>;
  rescan: (workspaceId: number) => Promise<void>;
  createBot: (name: string, instructions: string) => Promise<boolean>;
  selectBot: (slug: string) => void;
  newChat: () => void;
  openChat: (conversationId: string) => Promise<void>;
  send: (text: string) => Promise<void>;
  cancel: () => Promise<void>;
  respondApproval: (decision: ApprovalDecision) => Promise<void>;
  dismissNotice: () => void;
}

let unsubscribe: (() => void) | null = null;
let openingConvId: string | null = null;
let openBuffer: FaroEvent[] = [];
let openNonce = 0;

export const useStore = create<Store>((set, get) => {
  const fail = (err: unknown) => set({ notice: errorMessage(err) });

  async function refreshChats() {
    const chats = await bridge.listChats();
    set({ chats });
  }

  async function refresh() {
    const s = await bridge.getState();
    set((st) => ({
      workspaces: s.workspaces,
      bots: s.bots,
      botSlug: st.botSlug || s.defaultBot,
      view: s.hasKey ? "chat" : "onboarding",
    }));
  }

  function onEvent(ev: FaroEvent) {
    if (ev.type === "index.progress") {
      set((st) => ({
        index: {
          ...st.index,
          [ev.workspaceId]: {
            indexed: ev.indexed ?? 0,
            skipped: ev.skipped ?? 0,
            errors: ev.indexErrors ?? 0,
            done: !!ev.done,
            error: ev.error,
          },
        },
      }));
      if (ev.done) void refresh().catch(fail);
      return;
    }

    if (openingConvId !== null && ev.conversationId === openingConvId) {
      openBuffer.push(ev);
      if (ev.type === "turn.done" || ev.type === "turn.error" || ev.type === "turn.cancelled") {
        void refreshChats().catch(fail);
      }
      return;
    }

    set((st) => ({
      approval: applyApprovalEvent(st.approval, ev),
      chat:
        ev.type === "approval.request" || !st.conversationId || ev.conversationId !== st.conversationId
          ? st.chat
          : applyEvent(st.chat, ev),
    }));

    if (ev.type === "turn.done" || ev.type === "turn.error" || ev.type === "turn.cancelled") {
      void refreshChats().catch(fail);
    }
  }

  return {
    view: "loading",
    workspaces: [],
    bots: [],
    chats: [],
    botSlug: "",
    conversationId: null,
    index: {},
    chat: emptyChat,
    approval: null,
    chatReady: false,
    notice: null,

    async init() {
      unsubscribe?.(); // React StrictMode runs effects twice in dev
      unsubscribe = bridge.onEvent(onEvent);
      try {
        await refresh();
        await refreshChats();
      } catch (err) {
        fail(err);
      }
    },

    async refreshChats() {
      try {
        await refreshChats();
      } catch (err) {
        fail(err);
      }
    },

    async saveKey(key) {
      try {
        await bridge.saveApiKey(key);
        await refresh();
        await refreshChats();
        return true;
      } catch (err) {
        fail(err);
        return false;
      }
    },

    async addFolder() {
      try {
        const ws = await bridge.pickFolder();
        if (ws) await refresh();
      } catch (err) {
        fail(err);
      }
    },

    async rescan(workspaceId) {
      try {
        await bridge.rescan(workspaceId);
      } catch (err) {
        fail(err);
      }
    },

    async createBot(name, instructions) {
      try {
        const bot = await bridge.createBot(name, instructions);
        await refresh();
        get().selectBot(bot.slug);
        return true;
      } catch (err) {
        fail(err);
        return false;
      }
    },

    selectBot(slug) {
      if (isBusy(get().chat)) return;
      openNonce++;
      openingConvId = null;
      openBuffer = [];
      set({ botSlug: slug, conversationId: null, chat: emptyChat, chatReady: false });
    },

    newChat() {
      if (isBusy(get().chat)) return;
      openNonce++;
      openingConvId = null;
      openBuffer = [];
      set({ conversationId: null, chat: emptyChat, chatReady: false });
    },

    async openChat(conversationId) {
      const nonce = ++openNonce;
      openingConvId = conversationId;
      openBuffer = [];
      try {
        const snap = await bridge.openChat(conversationId);
        if (nonce !== openNonce) return;
        const buffered = openBuffer;
        openingConvId = null;
        openBuffer = [];
        set((st) => {
          const { chat, approval } = replaySnapshot(snap, buffered, st.approval);
          return {
            conversationId: snap.conversationId,
            botSlug: snap.botSlug,
            chat,
            approval,
            chatReady: true,
          };
        });
      } catch (err) {
        if (nonce === openNonce) {
          openingConvId = null;
          openBuffer = [];
        }
        fail(err);
      }
    },

    async send(text) {
      const trimmed = text.trim();
      if (!trimmed || isBusy(get().chat)) return;
      let convId = get().conversationId;
      if (!convId || !get().chatReady) {
        try {
          const info = await bridge.newChat(get().botSlug);
          convId = info.conversationId;
          set({ conversationId: convId, chatReady: true });
          void refreshChats().catch(fail);
        } catch (err) {
          fail(err);
          return;
        }
      }
      try {
        await bridge.send(convId, trimmed);
      } catch (err) {
        fail(err);
      }
    },

    async cancel() {
      try {
        await bridge.cancel();
      } catch (err) {
        fail(err);
      }
    },

    async respondApproval(decision) {
      const pending = get().approval;
      if (!pending) return;
      set({ approval: null }); // close at once so a double click cannot answer twice
      try {
        await bridge.respondApproval(pending.approvalId, decision);
      } catch (err) {
        fail(err);
      }
    },

    dismissNotice: () => set({ notice: null }),
  };
});

export const useBusy = () => useStore((s) => isBusy(s.chat));
