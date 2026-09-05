/** The Tasks screen: one wide list of everything owed, and under it a conversation about whichever task is picked. There is no list beside the sidebar — the sidebar is for chats — so the task the composer talks to is named in the header and chosen from the picker there. A task the user typed in can be ticked done and unticked open again; one Ora noticed in a meeting can also be dropped, which is the third state the store holds and the daemon takes on POST /tasks/{id}/done. */

import { useState, useRef } from "react";
import { ArrowRightLeft, Check, ChevronDown, ChevronRight, MoreHorizontal, Plus, RotateCcw, X } from "lucide-react";

import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from "@/components/ui/resizable";
import { useAllTasksQuery, useBrainsQuery, useConversationQuery, useCreateConversationMutation, useCreateTaskMutation, useSetTaskStatusMutation, type Task, type TaskStatus } from "./api";
import { shortWhen, taskContext, taskDetail, tasksShown } from "./format";
import { Composer, Thread } from "./chats";
import { HEAD, Nothing, PageHeader, Picker, Reading, Scroller, TAIL, BrainPicker, useFollowSelection, useWide } from "./parts";
import { ui, useAppDispatch, useAppSelector } from "./store";

/** The tick on a task, wherever a task is drawn: the Tasks list, the Days page's raised list, and the block of what a meeting left behind. Input: the task, as little of it as the tick needs so a day's raised item can use it too without carrying a whole Task. Output: the tick — an empty ring until it is done and a filled one after — which writes the new status straight away and says on one line when the daemon refuses it. */
export function TaskTick({ task }: { task: Pick<Task, "id" | "title" | "done"> }) {
  const dispatch = useAppDispatch();
  const [setStatus] = useSetTaskStatusMutation();
  return (
    <button
      type="button"
      role="checkbox"
      aria-checked={task.done}
      aria-label={task.done ? `Reopen ${task.title}` : `Mark ${task.title} done`}
      className="grid size-6 shrink-0 place-items-center rounded-sm outline-none hover:bg-hover focus-visible:ring-2 focus-visible:ring-ring"
      onClick={async (e) => {
        e.stopPropagation();
        try {
          await setStatus({ id: task.id, status: task.done ? "open" : "done" }).unwrap();
        } catch {
          dispatch(ui.noticed("Could not change that task"));
        }
      }}
    >
      <span
        className={`grid size-[15px] place-items-center rounded-full border transition-colors ${task.done ? "border-primary bg-primary text-primary-foreground" : "border-hairline-strong text-transparent"}`}
      >
        <Check className="size-2.5" strokeWidth={3} />
      </span>
    </button>
  );
}

/** One task in the list: the tick, the title on one line, where it came from and when it was raised in a column of their own on the right, and the menu holding the other status changes. Input: the task, whether it is the one the composer is aimed at, the moment the dates are read against, and whether this row also gets the small control that names how it would move between Mine and Theirs (only the Theirs section offers it, since a task already in Mine has nowhere more useful to go). Output: the row. */
function TaskRow({ task, selected, now, showOwnerControl }: { task: Task; selected: boolean; now: Date; showOwnerControl?: boolean }) {
  const dispatch = useAppDispatch();
  const [setStatus] = useSetTaskStatusMutation();
  const detail = taskDetail(task);
  const meta = [detail, task.when ? shortWhen(task.when, now) : ""].filter(Boolean).join(" · ");
  // Only an action item Ora noticed can be dropped: the daemon answers 400 for a dropped task of the user's own, because user_tasks has nowhere to hold a third state.
  const droppable = task.source === "noticed";

  const set = async (status: TaskStatus) => {
    try {
      await setStatus({ id: task.id, status }).unwrap();
    } catch {
      dispatch(ui.noticed("Could not change that task"));
    }
  };

  return (
    <div
      role="option"
      aria-selected={selected}
      data-row-id={task.id}
      tabIndex={0}
      onClick={() => dispatch(ui.taskOpened(task.id))}
      onKeyDown={(e) => {
        if (e.key !== "Enter" && e.key !== " ") return;
        e.preventDefault();
        dispatch(ui.taskOpened(task.id));
      }}
      className={`group flex h-8 items-center gap-2.5 rounded-sm px-2 outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring ${selected ? "bg-selected" : "hover:bg-hover"}`}
    >
      <TaskTick task={task} />
      <div className={`min-w-0 flex-1 truncate text-ui ${selected ? "font-medium" : ""} ${task.done ? "text-muted-foreground line-through" : ""}`} title={task.title}>
        {task.title}
      </div>
      {meta ? (
        <div className="hidden max-w-[26ch] shrink-0 truncate text-meta text-muted-foreground sm:block" title={meta}>
          {meta}
        </div>
      ) : null}
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            variant="ghost"
            size="icon-xs"
            aria-label={`More for ${task.title}`}
            className="text-muted-foreground opacity-0 group-hover:opacity-100 focus-visible:opacity-100 aria-expanded:opacity-100"
            onClick={(e) => e.stopPropagation()}
          >
            <MoreHorizontal />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" onClick={(e) => e.stopPropagation()}>
          {task.done ? (
            <DropdownMenuItem onClick={() => void set("open")}>
              <RotateCcw /> Reopen
            </DropdownMenuItem>
          ) : (
            <DropdownMenuItem onClick={() => void set("done")}>
              <Check /> Mark done
            </DropdownMenuItem>
          )}
          {droppable ? (
            <DropdownMenuItem variant="destructive" onClick={() => void set("dropped")}>
              <X /> Drop it
            </DropdownMenuItem>
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>
      {showOwnerControl ? <OwnerControl task={task} /> : null}
    </div>
  );
}

/** The control beside a watched row that names moving it between Mine and Theirs — read-only, because there is no route for it: POST /tasks/{id}/done only ever takes a status, nothing that touches who a task belongs to. Disabled rather than wired to a click that would only fail, with the route it needs in its title so the gap is visible rather than silent. Input: the task. Output: the button. */
function OwnerControl({ task }: { task: Task }) {
  return (
    <Button
      variant="ghost"
      size="icon-xs"
      disabled
      aria-label={task.owner === "me" ? `Move ${task.title} to theirs` : `Move ${task.title} to mine`}
      title="Ora has no way to move a task yet — needs a route such as PATCH /tasks/{id} that sets owner"
      className="text-muted-foreground opacity-0 group-hover:opacity-100 focus-visible:opacity-100 disabled:pointer-events-none"
    >
      <ArrowRightLeft />
    </Button>
  );
}

/** The box above the list where a new task is typed. Input: none. Output: the field; Enter adds the task and picks what the daemon named after it, so the composer is already aimed at the thing that was just written down. */
function NewTask() {
  const dispatch = useAppDispatch();
  const draft = useAppSelector((s) => s.ui.newTask);
  const [createTask] = useCreateTaskMutation();

  const add = async () => {
    const title = draft.trim();
    if (!title) return;
    dispatch(ui.taskTyped(""));
    try {
      const made = await createTask(title).unwrap();
      dispatch(ui.taskOpened(made.id));
    } catch {
      dispatch(ui.noticed("Could not add that task"));
    }
  };

  return (
    <div className="relative shrink-0">
      <Plus className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
      <Input
        value={draft}
        placeholder="Give Ora something to do"
        aria-label="Give Ora something to do"
        className="h-8 border-transparent bg-muted pl-8 text-ui"
        onChange={(e) => dispatch(ui.taskTyped(e.target.value))}
        onKeyDown={(e) => {
          if (e.key !== "Enter") return;
          e.preventDefault();
          void add();
        }}
      />
    </div>
  );
}

/** The section below Mine for a task raised by a meeting that is not clearly the user's own — collapsed behind a count by default, since a noticed item is something to watch rather than something already on the list. Input: the tasks not owned by the user that the current search still matches, how many there are with no search applied at all (which is what decides whether the section exists), who is picked, and the moment for their dates. Output: the disclosure, and the rows once it is opened; nothing at all when there is nothing being watched. */
function TheirsSection({ tasks, total, selectedId, now }: { tasks: Task[]; total: number; selectedId?: string; now: Date }) {
  const [open, setOpen] = useState(false);
  if (!total) return null;
  return (
    <div className="mt-2">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
        className="flex w-full items-center gap-1.5 rounded-sm px-2 py-1.5 text-left text-ui text-muted-foreground hover:bg-hover"
      >
        {open ? <ChevronDown className="size-3.5 shrink-0" /> : <ChevronRight className="size-3.5 shrink-0" />}
        <span>Theirs, watching</span>
        <span className="text-meta">({tasks.length})</span>
      </button>
      {open ? (
        tasks.length === 0 ? (
          <Nothing up empty="Nothing watching." />
        ) : (
          <div role="listbox" aria-label="Theirs, watching" className="-mx-2 flex flex-col">
            {tasks.map((t) => (
              <TaskRow key={t.id} task={t} selected={t.id === selectedId} now={now} showOwnerControl />
            ))}
          </div>
        )
      ) : null}
    </div>
  );
}

/** The Tasks screen. Input: none. Output: one page — the list above, the picked task's conversation below it, and the composer at the foot, which sends the task itself along with every question so the answer is about that task rather than about nothing. */
export function TasksScreen() {
  const dispatch = useAppDispatch();
  const { taskId, query, taskChats } = useAppSelector((s) => s.ui);
  const run = useAppSelector((s) => s.progress.run);
  const { data: tasks = [], isError } = useAllTasksQuery();
  const { data: brains = [] } = useBrainsQuery();
  const [createConversation] = useCreateConversationMutation();
  const [wide, pane] = useWide();
  const list = useRef<HTMLDivElement>(null);

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
        <ResizablePanel id="list" defaultSize="55" minSize="25">
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
                      <div ref={list} role="listbox" aria-label="Tasks" className="-mx-2 flex flex-col">
                        {mineTasks.map((t) => (
                          <TaskRow key={t.id} task={t} selected={t.id === selected?.id} now={now} />
                        ))}
                      </div>
                    )}
                    <TheirsSection tasks={theirs} total={theirsTotal} selectedId={selected?.id} now={now} />
                  </>
                )}
              </div>
            </Reading>
          </Scroller>
        </ResizablePanel>
        <ResizableHandle />
        <ResizablePanel id="about" defaultSize="45" minSize="25">
          <div className="flex h-full min-h-0 flex-col">
            <Thread
              view={view}
              up={!isError}
              wide={wide}
              sources={false}
              empty={selected ? `Nothing said about “${selected.title}” yet.` : "Pick a task above to ask about it."}
              hint={selected ? "Ask below and Ora answers with this task as the subject." : undefined}
              run={mine}
            />
            <Composer
              conversationId={conversationId}
              draftKey={selected?.id}
              brain={view?.brain}
              placeholder={selected ? "Say something about this task…" : "Pick a task first"}
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
