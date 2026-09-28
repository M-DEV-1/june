/** The Tasks screen, laid out as list and detail: everything owed in one column on the left, and on the right the picked task — its title, where it came from, the conversation about it, and the composer at the foot. Picking a row swaps the detail; there is no header picker and no split to drag, and the sidebar stays for chats. A task the user typed in can be ticked done and unticked open again; one June noticed in a meeting can also be dropped, which is the third state the store holds and the daemon takes on POST /tasks/{id}/done. */

import { useRef, type RefObject } from "react";

import { useAllTasksQuery, useBrainsQuery, useConversationQuery, useCreateConversationMutation, useMeetingsQuery } from "./api";
import { taskContext, taskMeeting, tasksShown } from "./format";
import { Composer, Thread } from "./chats";
import { HEAD, Nothing, PageHeader, Scroller, TAIL, BrainPicker, useFollowSelection } from "./parts";
import { TaskAbout } from "./task-about";
import { ui, useAppDispatch, useAppSelector } from "./store";
import { NewTask } from "./task-new";
import { TaskRow } from "./task-row";
import { TheirsSection } from "./task-theirs";

export { TaskTick } from "./task-tick";

/** The list column: everything the user owes, and under it what a meeting raised for somebody else. Input: which row is picked, the moment the dates are read against, and the list element itself, which the arrow keys move the focus inside. Output: the column. It reads the tasks and the search box from the store rather than taking them as props — useAllTasksQuery is the same cached read the screen above makes, so this costs no second fetch. */
function TaskList({ selectedId, now, rows }: { selectedId?: string; now: Date; rows: RefObject<HTMLUListElement | null> }) {
  const query = useAppSelector((s) => s.ui.query.tasks);
  const { data: tasks = [], isError, isLoading } = useAllTasksQuery();
  const shown = tasksShown(tasks, query);
  // Mine is the default view; Theirs holds what a meeting raised for someone else or for nobody named, which is watched rather than assumed onto the user's own list. theirsTotal ignores the search box, since whether the section exists at all should not flicker with what is typed into it.
  const mineTasks = shown.filter((t) => t.owner === "me");
  const theirs = shown.filter((t) => t.owner !== "me");
  const theirsTotal = tasks.filter((t) => t.owner !== "me").length;

  if (tasks.length === 0) return <Nothing up={!isError} loading={isLoading} empty="Nothing to do." />;
  return (
    <>
      {mineTasks.length === 0 ? (
        <Nothing up={!isError} loading={isLoading} empty={query ? `Nothing matches “${query}”.` : "Nothing of yours open."} />
      ) : (
        <ul ref={rows} role="list" aria-label="Tasks" className="-mx-2 flex flex-col">
          {mineTasks.map((t) => (
            <TaskRow key={t.id} task={t} selected={t.id === selectedId} now={now} />
          ))}
        </ul>
      )}
      <TheirsSection tasks={theirs} total={theirsTotal} selectedId={selectedId} now={now} />
    </>
  );
}

/** The Tasks screen. Input: none. Output: the list on the left and the picked task's detail on the right, whose composer sends the task itself along with every question so the answer is about that task rather than about nothing. */
export function TasksScreen() {
  const dispatch = useAppDispatch();
  const { taskId, query, taskChats } = useAppSelector((s) => s.ui);
  const run = useAppSelector((s) => s.progress.run);
  const { data: tasks = [], isError } = useAllTasksQuery();
  const { data: brains = [] } = useBrainsQuery();
  const { data: meetings = [] } = useMeetingsQuery();
  const [createConversation] = useCreateConversationMutation();
  const rows = useRef<HTMLUListElement>(null);

  const shown = tasksShown(tasks, query.tasks);
  const selected = tasks.find((t) => t.id === taskId) ?? shown[0];
  useFollowSelection(rows, selected?.id);
  // A task the app opened owns its conversation; one June noticed has none until it is asked about, and the one opened for it then is remembered here for the rest of the session.
  const conversationId = selected ? selected.conversation_id || taskChats[selected.id] || undefined : undefined;
  // currentData rather than data: RTK Query keeps the previous argument's result in data while the new one is still in flight, which showed the task just left under the task just picked.
  const { currentData: view } = useConversationQuery(conversationId ?? "", { skip: !conversationId });
  const now = new Date();
  const mine = run && conversationId && run.conversationId === conversationId ? run : undefined;
  const emptyLine = selected ? `Nothing said about “${selected.title}” yet.` : "Pick a task on the left to ask about it.";

  /** Opens the conversation a noticed task never had, named after the task, and remembers the pairing. Input: none. Output: the new conversation's id, or undefined when the daemon would not open one. */
  const startTaskChat = async (): Promise<string | undefined> => {
    if (!selected) return undefined;
    try {
      const made = await createConversation({ title: selected.title }).unwrap();
      dispatch(ui.taskChatOpened({ taskId: selected.id, conversationId: made.id }));
      return made.id;
    } catch {
      return undefined;
    }
  };

  return (
    <div data-pane className="flex h-full min-h-0 flex-col">
      <PageHeader full>
        <h1 className="shrink-0 text-ui font-medium">Tasks</h1>
        <div className="ml-auto">
          <BrainPicker current={view?.brain ?? ""} brains={brains} />
        </div>
      </PageHeader>

      <div className="flex min-h-0 flex-1">
        {/* ponytail: fixed 42% list column; a push-in list for panes under ~800px when someone actually runs it that narrow. */}
        <div className="flex w-[42%] min-w-[300px] max-w-[560px] shrink-0 flex-col border-r border-hairline">
          <Scroller bodyClassName={`${HEAD} ${TAIL} px-6`}>
            <div className="flex flex-col gap-3">
              <NewTask />
              <TaskList selectedId={selected?.id} now={now} rows={rows} />
            </div>
          </Scroller>
        </div>

        <section aria-label="About this task" className="flex min-w-0 flex-1 flex-col">
          <TaskAbout task={selected} meeting={taskMeeting(meetings, selected)} now={now} />
          <Thread
            view={view}
            up={!isError}
            sources={false}
            empty={emptyLine}
            hint={selected ? "Ask below and June answers with this task as the subject." : undefined}
            run={mine}
            newest={conversationId}
          />
          <Composer
            conversationId={conversationId}
            draftKey={selected?.id}
            brain={view?.brain}
            placeholder={selected ? "Say something about this task…" : "Pick a task first"}
            context={taskContext(selected)}
            start={selected ? startTaskChat : undefined}
          />
        </section>
      </div>
    </div>
  );
}
