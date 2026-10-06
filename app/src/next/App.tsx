/** The window itself: the rail on the left, whichever screen is showing beside it, and the three things that sit above every screen — the jump-to-a-chat palette, the rename box and the confirmation in front of a delete. The keys the whole window answers to are bound here as well: Ctrl+K opens the palette, Escape gives up whatever is half-done, and the arrows walk the list the screen showing has on its left. Until GET /setup says first-run setup is done, setup takes the whole window instead (onboarding.tsx), as does the screen shown while the daemon restarts; the strip offering a newer June sits above every screen (update.tsx), and so does the one saying June is paused (pause.tsx). */

import { useCallback, useEffect, useState } from "react";
import { useStore } from "react-redux";
import { Calendar, ListTodo, MessageSquare, Repeat, Settings as SettingsIcon, Video } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Command, CommandDialog, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList } from "@/components/ui/command";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar";
import { TooltipProvider } from "@/components/ui/tooltip";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "./alert-dialog";
import {
  errorStatus,
  juneApi,
  refreshToken,
  useConversationsQuery,
  useDaysQuery,
  useDeleteConversationMutation,
  useMeetingsQuery,
  useRenameConversationMutation,
  useSetupQuery,
  useTasksQuery,
} from "./api";
import { activeDays, chatsShown, daysShown, meetingsShown, shortWhen, step, tasksShown } from "./format";
import { useOpenAtOnShow } from "./openAt";
import { ChatsScreen } from "./chats";
import { TasksScreen } from "./tasks";
import { DaysScreen } from "./days";
import { MeetingsScreen } from "./meetings";
import { Onboarding, RestartScreen, useFirstQuestionAfterRestart } from "./onboarding";
import { RoutinesScreen } from "./routines";
import { SettingsScreen } from "./settings";
import { AppSidebar } from "./sidebar";
import { UpdateBanner } from "./update";
import { PausedBanner } from "./pause";
import { applyTheme } from "../shared/theme";
import { conversationsUi, escaped, settings, ui, useAppDispatch, useAppSelector, type Place, type RootState } from "./store";

/** How often GET /setup is asked again while it has not answered at all — the daemon still starting, on a fresh install's first open — so the window moves onto setup the moment it can say whether setup is needed. */
const SETUP_RETRY_MS = 2000;

// What the palette offers besides the chats: the five screens, so Ctrl+K reaches a page and not only a conversation. Module scope so it is not rebuilt on every render — it closes over nothing.
const PAGES: { place: Place; label: string; icon: typeof ListTodo }[] = [
  { place: "chats", label: "Chats", icon: MessageSquare },
  { place: "tasks", label: "Tasks", icon: ListTodo },
  { place: "meetings", label: "Meetings", icon: Video },
  { place: "days", label: "Days", icon: Calendar },
  { place: "routines", label: "Routines", icon: Repeat },
  { place: "settings", label: "Settings", icon: SettingsIcon },
];

/** The screen the place showing asks for. Input: none. Output: that screen. */
function Screen() {
  const place = useAppSelector((s) => s.ui.place);
  switch (place) {
    case "tasks":
      return <TasksScreen />;
    case "days":
      return <DaysScreen />;
    case "meetings":
      return <MeetingsScreen />;
    case "routines":
      return <RoutinesScreen />;
    case "settings":
      return <SettingsScreen />;
    default:
      return <ChatsScreen />;
  }
}

/** The whole window. Input: none. Output: the rail, the screen showing, and the three overlays. */
export default function App() {
  const dispatch = useAppDispatch();
  // One field at a time rather than the whole ui slice, and never the search text: the whole window re-rendered on every letter typed into a search field, which made typing the slowest thing in it.
  const place = useAppSelector((s) => s.ui.place);
  const conversationId = useAppSelector((s) => s.ui.conversationId);
  const taskId = useAppSelector((s) => s.ui.taskId);
  const date = useAppSelector((s) => s.ui.date);
  const meetingId = useAppSelector((s) => s.ui.meetingId);
  const paletteOpen = useAppSelector((s) => s.ui.paletteOpen);
  const store = useStore<RootState>();
  const { renamingId, draftTitle, confirmingDeleteId } = useAppSelector((s) => s.conversations);
  const theme = useAppSelector((s) => s.settings.theme);
  const restart = useAppSelector((s) => s.setup.restart);
  useOpenAtOnShow();

  const [setupUnanswered, setSetupUnanswered] = useState(true);
  const { data: setup, error: setupError } = useSetupQuery(undefined, { pollingInterval: setupUnanswered ? SETUP_RETRY_MS : 0 });
  // Any HTTP answer settles it, a 404 from a daemon too old to have setup included, which reads as setup long finished — except a refusal of this window's key, which the next try (reading the token afresh) may well get past.
  const setupStatus = errorStatus(setupError);
  const setupAnswered = setup !== undefined || (setupStatus !== undefined && setupStatus !== 401 && setupStatus !== 403);
  if (setupAnswered === setupUnanswered) setSetupUnanswered(!setupAnswered);
  const onboarding = setup?.done === false;
  // Until GET /setup has answered once the window cannot tell setup from Chats, and drawing Chats first would flash it in front of a fresh install's first screen. A daemon that is not there at all fails instead of answering, and then Chats draws with its own "Nothing is answering".
  const deciding = setup === undefined && setupError === undefined;
  // Setup and the restart screen cover the whole window, so the keys that walk the screen underneath it are not bound while either is up.
  const covered = Boolean(restart) || onboarding || deciding;
  // A restart takes the window down with it, so the first question setup ended on is asked here, by whichever window comes back. A feature's download needs nothing of the kind: the daemon keeps its queue in state.json and takes it up again itself (resume in internal/components).
  useFirstQuestionAfterRestart(setup?.done === true, Boolean(restart));

  const { data: convs = [] } = useConversationsQuery();
  const { data: tasks = [] } = useTasksQuery();
  const { data: days = [] } = useDaysQuery();
  const { data: meetings = [] } = useMeetingsQuery();
  const [rename] = useRenameConversationMutation();
  const [remove] = useDeleteConversationMutation();

  // The first conversation is what the window opens on, until the user picks another. Only while Chats is the screen showing: opening a conversation is also a way back to Chats, so doing it from Tasks or Settings would throw the user off the screen they asked for.
  useEffect(() => {
    if (place === "chats" && !conversationId && convs.length) dispatch(ui.conversationOpened(convs[0].id));
  }, [place, conversationId, convs, dispatch]);

  // The daemon writes a new token whenever it restarts, and it restarts this window as its child, so the token is read again every time the window is shown and everything on screen is read again with it.
  useEffect(() => {
    const onShown = () => {
      if (document.visibilityState !== "visible") return;
      void refreshToken().then(() => dispatch(juneApi.util.invalidateTags(["Conversation", "Task", "Day", "Meeting", "Settings", "Brain", "Usage", "Tracker", "Setup", "Component", "Update"])));
    };
    document.addEventListener("visibilitychange", onShown);
    window.addEventListener("focus", onShown);
    return () => {
      document.removeEventListener("visibilitychange", onShown);
      window.removeEventListener("focus", onShown);
    };
  }, [dispatch]);

  // Stamps the theme on the root element, which is what every colour token in index.css keys off. A stale resolution (see applyTheme's own themeAsk guard) comes back undefined and is not dispatched, so it cannot overwrite what a later, already-landed choice put in the store.
  useEffect(() => {
    void applyTheme(theme).then((r) => {
      if (r) dispatch(settings.themeResolved(r));
    });
  }, [theme, dispatch]);

  // What the arrows walk on the screen showing, in the order the rows appear, so the keys move through exactly what a search has left and skip what it hid. On Tasks that is the list on the page; on Meetings and Days it is what the header's picker offers, so the arrows step from one day or one recording to the next without opening the picker at all. Input: the search text, read off the store at the keypress. Output: the ids, the selected one, and the action that opens one.
  const walkOf = useCallback((query: RootState["ui"]["query"]) => {
    switch (place) {
      case "chats":
        return { ids: chatsShown(convs, query.chats).map((c) => c.id), selected: conversationId, open: (id: string) => ui.conversationOpened(id) };
      case "tasks":
        return { ids: tasksShown(tasks, query.tasks).map((t) => t.id), selected: taskId, open: (id: string) => ui.taskOpened(id) };
      case "days":
        return { ids: daysShown(activeDays(days), query.days).map((d) => d.date), selected: date, open: (id: string) => ui.dayOpened(id) };
      case "meetings":
        return { ids: meetingsShown(meetings, query.meetings).map((m) => m.id), selected: meetingId, open: (id: string) => ui.meetingOpened(id) };
      default:
        return { ids: [] as string[], selected: undefined, open: (id: string) => ui.conversationOpened(id) };
    }
  }, [place, convs, tasks, days, meetings, conversationId, taskId, date, meetingId]);

  useEffect(() => {
    if (covered) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "k" && (e.ctrlKey || e.metaKey)) {
        e.preventDefault();
        dispatch(ui.paletteToggled(undefined));
        return;
      }
      if (e.key === "Escape") {
        dispatch(escaped());
        return;
      }
      if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
      // An arrow in a text field belongs to the caret, except in a search field, where the point of typing was to pick something out of the list below it. A textarea (the composer) and anything editable in place own their arrows just the same as an <input> does — only there was never a search-field exception carved out for them, because there is no such thing as a search textarea.
      const active = document.activeElement as HTMLElement | null;
      if (active?.tagName === "TEXTAREA" || active?.isContentEditable) return;
      if (active?.tagName === "INPUT" && (active as HTMLInputElement).type !== "search") return;
      const walk = walkOf(store.getState().ui.query);
      const at = step(walk.ids.indexOf(walk.selected ?? ""), walk.ids.length, e.key);
      if (at < 0) return;
      e.preventDefault();
      dispatch(walk.open(walk.ids[at]));
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [dispatch, walkOf, store, covered]);

  const renaming = convs.find((c) => c.id === renamingId);
  const deleting = convs.find((c) => c.id === confirmingDeleteId);
  // The confirmation keeps naming the chat while it fades out: deleting goes undefined the instant it is answered, and the closing dialog read “”? for the length of its animation.
  const [deletingTitle, setDeletingTitle] = useState("");
  if (deleting && deleting.title !== deletingTitle) setDeletingTitle(deleting.title);
  const now = new Date();

  /** Writes a new title, and says on one line when the daemon would not take it. */
  const saveTitle = async () => {
    const title = draftTitle.trim();
    if (!renamingId || !title) return;
    dispatch(conversationsUi.renameEnded());
    try {
      await rename({ id: renamingId, title }).unwrap();
    } catch {
      dispatch(ui.noticed({ text: "Could not rename", kind: "error" }));
    }
  };

  /** Removes a conversation and, when it was the one open, moves to the next one the daemon still lists rather than leaving the pane pointed at turns that no longer have a conversation. Deleting the last one leaves no next, and an empty draft is what the window opens then: merely clearing conversationId would have the "keep some chat picked" effect above put the deleted id straight back, off the list RTK Query is still holding while its refetch is in flight, and the composer would go on posting to a conversation the daemon no longer has. A delete that does not go through leaves the row where it was, because the store still holds it, and says so on one line. */
  const confirmDelete = async () => {
    const id = confirmingDeleteId;
    if (!id) return;
    const next = convs.find((c) => c.id !== id)?.id;
    dispatch(conversationsUi.deleteConfirmed(undefined));
    try {
      await remove(id).unwrap();
      if (id === conversationId) dispatch(next ? ui.conversationOpened(next) : ui.chatDraftOpened());
    } catch {
      dispatch(ui.noticed({ text: "Could not delete", kind: "error" }));
    }
  };

  if (restart) {
    return (
      <TooltipProvider delayDuration={300}>
        <RestartScreen why={restart.why} slow={restart.slow} failed={restart.failed} />
      </TooltipProvider>
    );
  }
  if (onboarding) {
    return (
      <TooltipProvider delayDuration={300}>
        <Onboarding setup={setup} />
      </TooltipProvider>
    );
  }
  if (deciding) return <div className="h-svh bg-background" />;

  return (
    <TooltipProvider delayDuration={300}>
      {/* The window is exactly as tall as the desktop gives it and never grows past that: every screen inside scrolls its own reading region, and a long page must not push the composer or the header off the bottom. */}
      {/* 248px: wide enough for a chat title and the line under it, narrow enough that the reading column keeps the middle of the window. */}
      <SidebarProvider className="h-svh overflow-hidden" style={{ "--sidebar-width": "15.5rem" } as React.CSSProperties}>
        <AppSidebar />
        <SidebarInset className="flex min-h-0 min-w-0 flex-col">
          <UpdateBanner />
          <PausedBanner />
          <Screen />
        </SidebarInset>

        <CommandDialog open={paletteOpen} onOpenChange={(open) => dispatch(ui.paletteToggled(open))} title="Jump to a chat or a page" description="Search every chat by title, and every screen by name">
          <Command>
            <CommandInput placeholder="Search chats" />
            <CommandList>
              <CommandEmpty>Nothing by that name.</CommandEmpty>
              <CommandGroup heading="Go to">
                {PAGES.map((p) => (
                  <CommandItem
                    key={p.place}
                    value={`page ${p.label}`}
                    onSelect={() => {
                      dispatch(ui.placeShown(p.place));
                      dispatch(ui.paletteToggled(false));
                    }}
                  >
                    <p.icon className="size-4" />
                    <span>{p.label}</span>
                  </CommandItem>
                ))}
              </CommandGroup>
              <CommandGroup heading="Chats">
                {convs.map((c) => (
                  <CommandItem key={c.id} value={`${c.title} ${c.id}`} onSelect={() => dispatch(ui.conversationOpened(c.id))}>
                    <MessageSquare className="size-4" />
                    <span className="truncate" title={c.title}>
                      {c.title}
                    </span>
                    <span className="ml-auto shrink-0 text-meta text-muted-foreground">{shortWhen(c.updated, now)}</span>
                  </CommandItem>
                ))}
              </CommandGroup>
            </CommandList>
          </Command>
        </CommandDialog>

        <Dialog open={Boolean(renaming)} onOpenChange={(open) => !open && dispatch(conversationsUi.renameEnded())}>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>Rename chat</DialogTitle>
              <DialogDescription>A chat needs a title.</DialogDescription>
            </DialogHeader>
            <Input
              value={draftTitle}
              aria-label="New title"
              autoFocus
              onChange={(e) => dispatch(conversationsUi.draftTitleTyped(e.target.value))}
              onKeyDown={(e) => {
                if (e.key !== "Enter") return;
                e.preventDefault();
                void saveTitle();
              }}
            />
            <DialogFooter>
              <Button variant="ghost" onClick={() => dispatch(conversationsUi.renameEnded())}>
                Cancel
              </Button>
              <Button disabled={!draftTitle.trim()} onClick={() => void saveTitle()}>
                Rename
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>

        <AlertDialog open={Boolean(deleting)} onOpenChange={(open) => !open && dispatch(conversationsUi.deleteConfirmed(undefined))}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>Delete “{deleting?.title ?? deletingTitle}”?</AlertDialogTitle>
              <AlertDialogDescription>This removes the chat and every turn said in it. There is no undo.</AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>Keep it</AlertDialogCancel>
              <AlertDialogAction onClick={() => void confirmDelete()}>Delete</AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </SidebarProvider>
    </TooltipProvider>
  );
}
