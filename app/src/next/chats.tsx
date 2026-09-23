/** The Chats screen: the open conversation's turns, the question in flight while it runs, and the composer where the next one is typed. The thread and the composer are exported because the Tasks screen talks to a task through the same two pieces, with the task itself sent along as context. */

import {
  useBrainsQuery,
  useConversationQuery,
  useConversationsQuery,
  useSettingsQuery,
} from "./api";
import {
  HEAD,
  MEASURE,
  PageHeader,
  Scroller,
  TAIL,
  BrainPicker,
  useWide,
} from "./parts";
import { NoBrainPanel } from "./settings";
import { DRAFT_CHAT, useAppSelector } from "./store";
import { Composer } from "./chat-composer";
import { Thread } from "./chat-turn";
import { ChatSuggestions } from "./chat-welcome";
import { greeting } from "./format";

export { Composer, Thread };

/** The Chats screen. Input: none — the conversation showing and the question in flight both come from the store. Output: the header, the thread and the composer, on one page with nothing beside it. */
export function ChatsScreen() {
  const conversationId = useAppSelector((s) => s.ui.conversationId);
  const chatDraft = useAppSelector((s) => s.ui.chatDraft);
  const run = useAppSelector((s) => s.progress.run);
  const jobs = useAppSelector((s) => s.progress.jobs);
  // A chat draft shows as though nothing were picked, even though conversationId still names whatever was open before "New chat" — cleared would only have App.tsx's own "keep some chat picked" effect put it straight back the instant one exists.
  const shownId = chatDraft ? undefined : conversationId;
  const {
    data: convs = [],
    isError,
    isLoading: listLoading,
  } = useConversationsQuery();
  // currentData rather than data: RTK Query keeps the previous arg's result in data while a skipped query stays uninitialized, which would leave a draft showing the conversation it was opened over instead of nothing.
  const { currentData: view, isLoading } = useConversationQuery(shownId ?? "", {
    skip: !shownId,
  });
  const { data: brains = [] } = useBrainsQuery();
  // Nothing typed can be answered while no brain is signed in, so the pane says so instead of showing the thread. An empty list is a daemon that said nothing, not one that has no brain: a real one always lists every brain it knows.
  const noBrain = brains.length > 0 && !brains.some((b) => b.signed_in);
  const { data: daemon } = useSettingsQuery();
  const [wide, pane] = useWide();

  const current = convs.find((c) => c.id === shownId);
  // In a wide pane the rail's track is always reserved, filled or not, so the header, the thread and the composer sit at the same place in every chat: a column that moved left the moment a reply called a tool, and back when the next chat had none, read as the page jumping about (2026-09-05).
  const railed = wide;
  // A fresh draft has no conversationId yet, tracked under this sentinel until the first message sent from it opens a real one and the composer's own send() moves everything over to that id.
  const key = shownId ?? DRAFT_CHAT;
  const mine = run && run.conversationId === key ? run : undefined;
  const mineJob = jobs[key];

  return (
    <div ref={pane} data-pane className="flex h-full min-h-0 flex-col">
      <PageHeader wide={wide} railed={railed}>
        <h1 className="truncate text-ui font-medium" title={current?.title}>
          {current?.title ?? "Ora"}
        </h1>
        <div className="ml-auto">
          <BrainPicker
            current={view?.brain ?? current?.brain ?? ""}
            brains={brains}
          />
        </div>
      </PageHeader>
      {!noBrain ? (
        <Thread
          view={view}
          up={!isError}
          loading={isLoading || listLoading}
          // A chat that exists and is empty says so. A fresh draft is the front door instead: the time of day, one line saying Ora has been keeping track, and a few things it can do.
          empty={shownId ? "Nothing said in this chat yet." : greeting()}
          hint={
            shownId
              ? "Ask a question below and Ora answers from what it has seen and heard."
              : "I have been keeping track. What would you like to do?"
          }
          action={shownId ? undefined : <ChatSuggestions />}
          run={mine}
          job={mineJob}
          wide={wide}
          newest={key}
        />
      ) : (
        <Scroller bodyClassName={`${HEAD} ${TAIL}`}>
          <div className={MEASURE}>
            <NoBrainPanel dataDir={daemon?.data_dir ?? "~/.local/share/ora"} />
          </div>
        </Scroller>
      )}
      <Composer
        conversationId={shownId}
        draftKey={key}
        brain={view?.brain}
        wide={wide}
        railed={railed}
        fresh={!shownId}
      />
    </div>
  );
}
