/** The Tasks screen: one wide list of everything owed, and under it a conversation about whichever task is picked. There is no list beside the sidebar — the sidebar is for chats — so the task the composer talks to is named in the header and chosen from the picker there. A task the user typed in can be ticked done and unticked open again; one Ora noticed in a meeting can also be dropped, which is the third state the store holds and the daemon takes on POST /tasks/{id}/done. */

import { useRef } from "react";

import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from "@/components/ui/resizable";
import { useAllTasksQuery, useBrainsQuery, useConversationQuery, useCreateConversationMutation } from "./api";
import { shortWhen, taskContext, tasksShown } from "./format";
import { Composer, Thread } from "./chats";
import { HEAD, Nothing, PageHeader, Picker, Reading, Scroller, TAIL, BrainPicker, useFollowSelection, useWide } from "./parts";
import { ui, useAppDispatch, useAppSelector } from "./store";
import { NewTask } from "./task-new";
import { TaskRow } from "./task-row";
import { TheirsSection } from "./task-theirs";

export { TaskTick } from "./task-tick";

/** The Tasks screen. Input: none. Output: one page — the list above, the picked task's conversation below it, and the composer at the foot, which sends the task itself along with every question so the answer is about that task rather than about nothing. */
export function TasksScreen() {
  const dispatch = useAppDispatch();
  const { taskId, query, taskChats } = useAppSelector((s) => s.ui);
  const run = useAppSelector((s) => s.progress.run);
  const { data: tasks = [], isError } = useAllTasksQuery();
  const { data: brains = [] } = useBrainsQuery();
  const [createConversation] = useCreateConversationMutation();
  const [wide, pane] = useWide();
  const list = useRef<HTMLUListElement>(null);

  const shown = tasksShown(tasks, query.tasks);
  // Mine is the default view; Theirs holds what a meeting raised for someone else or for nobody named, which is watched rather than assumed onto the user's own list. theirsTotal ignores the search box, since whether the section exists at all should not flicker with what is typed into it.
  const mineTasks = shown.filter((t) => t.owner === "me");
  const theirs = shown.filter((t) => t.owner !== "me");
  const theirsTotal = tasks.filter((t) => t.owner !== "me").length;
  const selected = tasks.find((t) => t.id === taskId) ?? shown[0];
  useFollowSelection(list, selected?.id);
  // A task the app opened owns its conversation; one Ora noticed has none until it is asked about, and the one opened for it then is remembered here for the rest of the session.
  const conversationId = selected ? selected.conversation_id || taskChats[selected.id] || undefined : undefined;
  const { data: view } = useConversationQuery(conversationId ?? "", { skip: !conversationId });
  const now = new Date();
  const mine = run && conversationId && run.conversationId === conversationId ? run : undefined;
  const emptyLine = selected ? `Nothing said about “${selected.title}” yet.` : "Pick a task above to ask about it.";
  const hasTalked = Boolean(view?.turns?.length) || Boolean(mine);

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

  const options = shown.map((t) => ({
    id: t.id,
    label: t.title,
    hint: t.when ? shortWhen(t.when, now) : "",
    group: t.source === "you" ? "You set" : "Ora noticed",
  }));

  return (
    <div ref={pane} data-pane className="flex h-full min-h-0 flex-col">
      <PageHeader wide={wide}>
        <h1 className="shrink-0 text-ui font-medium">Tasks</h1>
        <Picker
          list="tasks"
          label="Choose a task"
          placeholder="Search tasks"
          options={options}
          selected={selected?.id}
          empty={tasks.length ? `Nothing matches “${query.tasks}”.` : "Nothing to do."}
          onPick={(id) => dispatch(ui.taskOpened(id))}
        />
        <div className="ml-auto">
          <BrainPicker current={view?.brain ?? ""} brains={brains} />
        </div>
      </PageHeader>

      <ResizablePanelGroup orientation="vertical" className="min-h-0 flex-1">
        <ResizablePanel id="list" defaultSize="70" minSize="30">
          <Scroller bodyClassName={`${HEAD} ${TAIL}`}>
            <Reading wide={wide}>
              <div className="flex flex-col gap-3">
                <NewTask />
                {tasks.length === 0 ? (
                  <Nothing up={!isError} empty="Nothing to do." />
                ) : (
                  <>
                    {mineTasks.length === 0 ? (
                      <Nothing up={!isError} empty={query.tasks ? `Nothing matches “${query.tasks}”.` : "Nothing of yours open."} />
                    ) : (
                      <ul ref={list} role="list" aria-label="Tasks" className="-mx-2 flex flex-col">
                        {mineTasks.map((t) => (
                          <TaskRow key={t.id} task={t} selected={t.id === selected?.id} now={now} />
                        ))}
                      </ul>
                    )}
                    <TheirsSection tasks={theirs} total={theirsTotal} selectedId={selected?.id} now={now} />
                  </>
                )}
              </div>
            </Reading>
          </Scroller>
        </ResizablePanel>
        <ResizableHandle withHandle />
        <ResizablePanel id="about" defaultSize="30" minSize="10" collapsible collapsedSize={0}>
          {/* A task with nothing said about it yet gets no centred empty state down here — the composer alone, with the same sentence as its placeholder, is the whole panel. */}
          <div className="flex h-full min-h-0 flex-col justify-end">
            {hasTalked ? (
              <Thread
                view={view}
                up={!isError}
                wide={wide}
                sources={false}
                empty={emptyLine}
                hint={selected ? "Ask below and Ora answers with this task as the subject." : undefined}
                run={mine}
              />
            ) : null}
            <Composer
              conversationId={conversationId}
              draftKey={selected?.id}
              brain={view?.brain}
              placeholder={hasTalked ? (selected ? "Say something about this task…" : "Pick a task first") : emptyLine}
              context={taskContext(selected)}
              start={selected ? startTaskChat : undefined}
              wide={wide}
            />
          </div>
        </ResizablePanel>
      </ResizablePanelGroup>
    </div>
  );
}
