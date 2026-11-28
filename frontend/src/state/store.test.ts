import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Snapshot } from "../lib/events";

interface MockState {
  hasKey: boolean;
  defaultBot: string;
  workspaces: { id: number; path: string; name: string; fileCount: number }[];
  bots: { id: string; slug: string; name: string }[];
}

const mockBridge = vi.hoisted(() => ({
  getState: vi.fn<() => Promise<MockState>>(),
  saveApiKey: vi.fn<(key: string) => Promise<void>>(),
  pickFolder: vi.fn(),
  rescan: vi.fn(),
  createBot: vi.fn(),
  botChat: vi.fn<(slug: string) => Promise<{ conversationId: string; botSlug: string }>>(),
  openChat: vi.fn<(convId: string) => Promise<Snapshot>>(),
  send: vi.fn(),
  cancel: vi.fn(),
  respondApproval: vi.fn(),
  onEvent: vi.fn(() => () => {}),
}));

vi.mock("../lib/bridge", () => ({
  bridge: mockBridge,
  errorMessage: (err: unknown) => (err instanceof Error ? err.message : String(err)),
}));

import { useStore } from "./store";
import { emptyChat } from "./chat";

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

beforeEach(() => {
  vi.clearAllMocks();
  useStore.setState({
    view: "loading",
    workspaces: [],
    bots: [],
    botSlug: "",
    conversationId: null,
    index: {},
    chat: emptyChat,
    approval: null,
    chatReady: false,
    notice: null,
  });
});

describe("app store", () => {
  it("does not call botChat or show an error notice on first launch without an API key", async () => {
    mockBridge.getState.mockResolvedValue({
      hasKey: false,
      defaultBot: "faro",
      workspaces: [],
      bots: [{ id: "bot_1", slug: "faro", name: "Faro" }],
    });

    await useStore.getState().init();

    expect(useStore.getState().view).toBe("onboarding");
    expect(useStore.getState().notice).toBeNull();
    expect(mockBridge.botChat).not.toHaveBeenCalled();
  });

  it("ignores stale responses and stale openChat errors when switching bots quickly", async () => {
    const botA = deferred<{ conversationId: string; botSlug: string }>();
    const snapA = deferred<Snapshot>();
    const snapB = deferred<Snapshot>();

    mockBridge.botChat.mockImplementation((slug) => {
      if (slug === "alpha") return botA.promise;
      return Promise.resolve({ conversationId: "conv_b", botSlug: "beta" });
    });
    mockBridge.openChat.mockImplementation((id) => {
      if (id === "conv_a") return snapA.promise;
      return snapB.promise;
    });

    const pA = useStore.getState().selectBot("alpha");
    botA.resolve({ conversationId: "conv_a", botSlug: "alpha" });
    await Promise.resolve();

    const pB = useStore.getState().selectBot("beta");
    snapB.resolve({ conversationId: "conv_b", botSlug: "beta", busy: false, lastSeq: 1, events: [] });
    await pB;

    // Now the earlier alpha openChat rejects; it must not clobber beta's state or show a stale notice.
    snapA.reject(new Error("stale failure"));
    await pA;

    expect(useStore.getState().botSlug).toBe("beta");
    expect(useStore.getState().conversationId).toBe("conv_b");
    expect(useStore.getState().chatReady).toBe(true);
    expect(useStore.getState().notice).toBeNull();
  });
});
