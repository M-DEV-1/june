/** The one rail every screen shares: New chat, a search over the conversations, the conversations themselves under their date headings with a menu on each row, and the rows at the foot that lead to Tasks, Meetings, Days and Settings. Clicking a conversation from any screen is the way back to Chats, and clicking the lit foot row is the way back too, which is the behaviour the current window settled on. */

import { useEffect, useMemo, useRef, useState } from "react";
import { Calendar, ListTodo, MoreHorizontal, Pencil, Plus, Repeat, Search, Settings as SettingsIcon, Trash2, Video } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuAction,
  SidebarMenuButton,
  SidebarMenuItem,
} from "@/components/ui/sidebar";
import { useActOnNoticeMutation, useConversationsQuery } from "./api";
import { chatsShown, groupConversations, shortWhen } from "./format";
import { useFollowSelection } from "./parts";
import { conversationsUi, ui, useAppDispatch, useAppSelector, type Place } from "./store";

/** One row at the foot of the rail. */
type Foot = { place: Place; label: string; icon: typeof ListTodo };

// The four places that are not a chat, fixed regardless of what is in the store — module scope so it is not rebuilt on every render (the token ledger is not among them: it is the last section of Settings).
const FEET: Foot[] = [
  { place: "tasks", label: "Tasks", icon: ListTodo },
  { place: "meetings", label: "Meetings", icon: Video },
  { place: "days", label: "Days", icon: Calendar },
  { place: "routines", label: "Routines", icon: Repeat },
  { place: "settings", label: "Settings", icon: SettingsIcon },
];

/** What one of a live notice's buttons is called out loud. Input: the word on the button and the notice's own title, which is stored but never drawn. Output: the two joined, or the word alone for a notice with no title. */
function noticeLabel(word: string, title: string): string {
  return title ? `${word} — ${title}` : word;
}

/** The rail. Input: none — everything it draws comes from the store and the conversations cache. Output: the sidebar element, which SidebarProvider in App.tsx places. */
/** Short labels for the buttons a notice can carry, so the rail keeps its own compact wording for the answers it knows and still draws anything else the daemon names. */
const NOTICE_LABELS: Record<string, string> = { done: "Done", hour: "1 h", evening: "Evening", tomorrow: "Tomorrow" };

/** The buttons the rail draws for a live notice. Input: the actions the notice named, which the daemon decides — a task and the stale-task question carry their own, and a notice with nothing to complete carries only Open (see openOnlyActions and noticeActions in internal/proactive). Output: key and label per button, dropping Open, which the app window is already the answer to. Before this, both windows drew Done and three snoozes for every notice, so a routine's report or a transcription offered four buttons the daemon then refused with "Could not do that". */
function noticeButtons(actions: { key: string; label: string }[] | undefined): { key: string; label: string }[] {
  return (actions ?? [])
    .filter(({ key }) => key !== "default" && key !== "open")
    .map(({ key, label }) => ({ key, label: NOTICE_LABELS[key] ?? label }));
}

export function AppSidebar() {
  const dispatch = useAppDispatch();
  const { place, conversationId, query, notice, liveNotice } = useAppSelector((s) => s.ui);
  const { data: convs = [], isFetching, isLoading, isError } = useConversationsQuery();
  const [actOnNotice] = useActOnNoticeMutation();
  const list = useRef<HTMLDivElement>(null);

  /** Whether the last press on the live notice's buttons did not go through. Cleared whenever the card is showing a different notice, so the line belongs to the notice it was said about and to no other. */
  const [pressFailed, setPressFailed] = useState(false);
  useEffect(() => setPressFailed(false), [liveNotice?.kind, liveNotice?.id]);

  /** Presses one of the live notice's own buttons. The daemon's answer comes back as the same "notice" event a desktop press produces (see reactToNotice in store.ts), which is what replaces these buttons with the rail line's plain text. A refusal sends no such event — the daemon answers 404 for a notice whose task has already been closed elsewhere and returns before it would echo anything — so it is said as one quiet line on the card itself (DESIGN.md rule 18), which stays up with its buttons: the press is what failed, not the notice, and the hover window keeps its own card up for the same reason (noticeFailed in src/main.ts). */
  const act = (action: string) => {
    if (!liveNotice) return;
    setPressFailed(false);
    void actOnNotice({ ...liveNotice, action })
      .unwrap()
      .catch(() => setPressFailed(true));
  };

  const now = new Date();
  const today = now.toDateString();
  const groups = useMemo(() => groupConversations(chatsShown(convs, query.chats), new Date(today)), [convs, query.chats, today]);
  useFollowSelection(list, conversationId);

  /** Opens an unsaved draft rather than a conversation the daemon has to be told to make: nothing is posted and nothing appears in the list until the draft's own first message opens a real one, the same way a noticed task's first question does. */
  const newChat = () => dispatch(ui.chatDraftOpened());

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader className="gap-2 p-2">
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton onClick={() => void newChat()} tooltip="New chat" className="h-8 bg-card font-medium shadow-sm hover:bg-card hover:text-primary [&_svg]:text-primary">
              <Plus />
              <span>New chat</span>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
        <div className="relative group-data-[collapsible=icon]:hidden">
          <Search className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input
            type="search"
            value={query.chats}
            onChange={(e) => dispatch(ui.searched({ list: "chats", text: e.target.value }))}
            placeholder="Search chats"
            aria-label="Search chats"
            className="h-8 rounded-full border-transparent bg-sidebar-accent pl-8 text-ui"
          />
        </div>
        {liveNotice ? (
          <div className="flex flex-col gap-1 px-1 group-data-[collapsible=icon]:hidden">
            <p role="status" className="text-meta text-muted-foreground">
              {liveNotice.body}
            </p>
            {/* The four words on their own tell a screen reader nothing about what is being done or snoozed, so each button's own label names the notice it belongs to. */}
            <div className="flex gap-1">
              {noticeButtons(liveNotice.actions).map(({ key, label }) => (
                <Button key={key} variant="outline" size="xs" aria-label={noticeLabel(label, liveNotice.title)} onClick={() => act(key)}>
                  {label}
                </Button>
              ))}
            </div>
            {pressFailed ? (
              <p role="status" className="text-meta text-muted-foreground">
                Could not do that
              </p>
            ) : null}
          </div>
        ) : notice ? (
          <p role="status" className="px-1 text-meta text-destructive group-data-[collapsible=icon]:hidden">
            {notice}
          </p>
        ) : null}
      </SidebarHeader>

      <SidebarContent>
        {/* Radix wraps what a viewport holds in a table box sized to its widest row, which is wider than the rail whenever a subtitle runs long; the row then clips at the rail's own edge instead of at the truncated span's, so no ellipsis ever shows. Pinning that box's own width to zero and its minimum width to the full rail (the same box the comment in parts.tsx's Scroller names) keeps it from growing past the rail, so the too-long row is what overflows and truncates, not the box around it. */}
        <ScrollArea ref={list} className="h-full [&>[data-radix-scroll-area-viewport]>div]:table-fixed [&>[data-radix-scroll-area-viewport]>div]:w-0 [&>[data-radix-scroll-area-viewport]>div]:min-w-full">
          {isLoading ? (
            <div className="flex flex-col gap-3 p-3 group-data-[collapsible=icon]:hidden" aria-hidden>
              {[0, 1, 2, 3, 4].map((i) => (
                <div key={i} className="flex flex-col gap-1.5">
                  <Skeleton className="h-3 w-[70%]" />
                  <Skeleton className="h-2.5 w-[45%]" />
                </div>
              ))}
            </div>
          ) : null}
          {groups.map((g) => (
            <SidebarGroup key={g.label} role="group" aria-label={g.label} className="gap-0.5 px-2 py-1">
              {/* Sentence case at the metadata size, in the muted colour: a date marker in a list, not a form's field label. Small grey capitals are for a keycap and nothing else. */}
              <SidebarGroupLabel className="h-7 px-2 text-meta font-normal text-muted-foreground">{g.label}</SidebarGroupLabel>
              <SidebarGroupContent>
                <SidebarMenu>
                  {g.items.map((c) => {
                    const line = `${shortWhen(c.updated, now)}${c.last ? ` · ${c.last}` : ""}`;
                    return (
                      <SidebarMenuItem key={c.id}>
                        <SidebarMenuButton
                          isActive={c.id === conversationId}
                          tooltip={c.title}
                          data-row-id={c.id}
                          onClick={() => dispatch(ui.conversationOpened(c.id))}
                          // A conversation lit while another screen is showing says which chat is waiting, not which screen you are on, so off Chats it is marked faintly.
                          className={`h-auto flex-col items-start gap-0.5 rounded-sm px-2 py-1.5 data-active:bg-selected ${c.id === conversationId && place !== "chats" ? "font-normal data-active:bg-selected/50" : ""}`}
                        >
                          <span className="w-full truncate text-ui" title={c.title}>
                            {c.title}
                          </span>
                          <span className="w-full truncate text-meta text-muted-foreground" title={line}>
                            {line}
                          </span>
                        </SidebarMenuButton>
                        <DropdownMenu>
                          <DropdownMenuTrigger asChild>
                            <SidebarMenuAction aria-label={`More for ${c.title}`} className="top-2 text-muted-foreground opacity-0 hover:opacity-100 focus-visible:opacity-100 data-open:opacity-100">
                              <MoreHorizontal />
                            </SidebarMenuAction>
                          </DropdownMenuTrigger>
                          <DropdownMenuContent side="right" align="start">
                            <DropdownMenuItem onClick={() => dispatch(conversationsUi.renameStarted({ id: c.id, title: c.title }))}>
                              <Pencil /> Rename
                            </DropdownMenuItem>
                            <DropdownMenuItem variant="destructive" onClick={() => dispatch(conversationsUi.deleteConfirmed(c.id))}>
                              <Trash2 /> Delete
                            </DropdownMenuItem>
                          </DropdownMenuContent>
                        </DropdownMenu>
                      </SidebarMenuItem>
                    );
                  })}
                </SidebarMenu>
              </SidebarGroupContent>
            </SidebarGroup>
          ))}
          {!isFetching && groups.length === 0 ? (
            <p className="px-4 py-8 text-ui text-muted-foreground group-data-[collapsible=icon]:hidden">
              {isError ? "Not connected." : convs.length ? `Nothing matches “${query.chats}”.` : "No chats yet."}
            </p>
          ) : null}
        </ScrollArea>
      </SidebarContent>

      <SidebarFooter className="gap-0 border-t p-2">
        <SidebarMenu>
          {FEET.map((f) => (
            <SidebarMenuItem key={f.place}>
              <SidebarMenuButton
                isActive={place === f.place}
                tooltip={f.label}
                onClick={() => dispatch(ui.footToggled(f.place))}
                aria-pressed={place === f.place}
                className="h-8 rounded-sm text-muted-foreground data-active:bg-selected data-active:text-foreground"
              >
                <f.icon />
                <span>{f.label}</span>
              </SidebarMenuButton>
            </SidebarMenuItem>
          ))}
        </SidebarMenu>
      </SidebarFooter>
    </Sidebar>
  );
}
