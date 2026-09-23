/** The one rail every screen shares: New chat, a search over the conversations, the conversations themselves under their date headings with a menu on each row, and the rows at the foot that lead to Tasks, Meetings, Days and Settings. Clicking a conversation from any screen is the way back to Chats, and clicking the lit foot row is the way back too, which is the behaviour the current window settled on. */

import { useEffect, useMemo, useRef, useState } from "react";
import { Calendar, ListTodo, MoreHorizontal, Pencil, Plus, Repeat, Search, Settings as SettingsIcon, Trash2, Video, X } from "lucide-react";

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
import { useActOnNoticeMutation, useConversationsQuery, type ConversationSummary } from "./api";
import { chatsShown, groupConversations, noticeAge, shortWhen } from "./format";
import { useFollowSelection } from "./parts";
import { Face, useOraState } from "./face";
import { RunningNow } from "./running-now";
import { conversationsUi, ui, useAppDispatch, useAppSelector, type LiveNotice, type Place } from "./store";

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

/** One conversation in the rail: its title, the line saying when it was last touched and what was said, and the menu that renames or deletes it. Input: the conversation, whether it is the one open, whether Chats is the screen showing, and the moment the dates are read against. Output: the row. */
function ChatRow({ conv, open, onChats, now }: { conv: ConversationSummary; open: boolean; onChats: boolean; now: Date }) {
  const dispatch = useAppDispatch();
  const line = `${shortWhen(conv.updated, now)}${conv.last ? ` · ${conv.last}` : ""}`;
  return (
    <SidebarMenuItem>
      <SidebarMenuButton
        isActive={open}
        tooltip={conv.title}
        data-row-id={conv.id}
        onClick={() => dispatch(ui.conversationOpened(conv.id))}
        // A conversation lit while another screen is showing says which chat is waiting, not which screen you are on, so off Chats it is marked faintly.
        className={`h-auto flex-col items-start gap-0.5 rounded-sm px-2 py-1.5 data-active:bg-selected ${open && !onChats ? "font-normal data-active:bg-selected/50" : ""}`}
      >
        <span className="w-full truncate text-ui" title={conv.title}>
          {conv.title}
        </span>
        <span className="w-full truncate text-meta text-muted-foreground" title={line}>
          {line}
        </span>
      </SidebarMenuButton>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <SidebarMenuAction aria-label={`More for ${conv.title}`} className="top-2 text-muted-foreground opacity-0 hover:opacity-100 focus-visible:opacity-100 data-open:opacity-100">
            <MoreHorizontal />
          </SidebarMenuAction>
        </DropdownMenuTrigger>
        <DropdownMenuContent side="right" align="start">
          <DropdownMenuItem onClick={() => dispatch(conversationsUi.renameStarted({ id: conv.id, title: conv.title }))}>
            <Pencil /> Rename
          </DropdownMenuItem>
          <DropdownMenuItem variant="destructive" onClick={() => dispatch(conversationsUi.deleteConfirmed(conv.id))}>
            <Trash2 /> Delete
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </SidebarMenuItem>
  );
}

/** The grey lines standing in for the chat list while the first read is in flight. Input: none. Output: five rows of skeleton, hidden to a screen reader since there is nothing to read yet. */
function ChatListSkeleton() {
  return (
    <div className="flex flex-col gap-3 p-3 group-data-[collapsible=icon]:hidden" aria-hidden>
      {[0, 1, 2, 3, 4].map((i) => (
        <div key={i} className="flex flex-col gap-1.5">
          <Skeleton className="h-3 w-[70%]" />
          <Skeleton className="h-2.5 w-[45%]" />
        </div>
      ))}
    </div>
  );
}

/** One day's worth of conversations under its own heading. Input: the group, which conversation is open, whether Chats is the screen showing, and the moment the dates are read against. Output: the group. */
function ChatGroup({ group, conversationId, onChats, now }: { group: { label: string; items: ConversationSummary[] }; conversationId?: string; onChats: boolean; now: Date }) {
  return (
    <SidebarGroup role="group" aria-label={group.label} className="gap-0.5 px-2 py-1">
      {/* Sentence case at the metadata size, in the muted colour: a date marker in a list, not a form's field label. Small grey capitals are for a keycap and nothing else. */}
      <SidebarGroupLabel className="h-7 px-2 text-meta font-normal text-muted-foreground">{group.label}</SidebarGroupLabel>
      <SidebarGroupContent>
        <SidebarMenu>
          {group.items.map((c) => (
            <ChatRow key={c.id} conv={c} open={c.id === conversationId} onChats={onChats} now={now} />
          ))}
        </SidebarMenu>
      </SidebarGroupContent>
    </SidebarGroup>
  );
}

/** The card the live notice is said on, between the search field and the list of chats. Input: the notice and the moment its age is read against. Output: the card.
 *
 * A card rather than loose text: a bare sentence with four buttons under it reads as part of neither of its neighbours. Laid out the way the design sheets of 2026-09-12 draw a notification: the face, then "Ora" with how long ago it landed, then the line, then the detail under it. Said by someone, in other words, rather than posted by the window — which is the whole difference between a notice and a banner. The body is held to three lines for the same reason the hover card holds it to three: a morning brief is a paragraph, and unclamped it pushed the chat list down the rail.
 *
 * The daemon's answer to a press comes back as the same "notice" event a desktop press produces (see reactToNotice in store.ts), which is what replaces these buttons with the rail line's plain text. A refusal sends no such event — the daemon answers 404 for a notice whose task has already been closed elsewhere and returns before it would echo anything — so it is said as one quiet line on the card itself (DESIGN.md rule 18), which stays up with its buttons: the press is what failed, not the notice, and the hover window keeps its own card up for the same reason (noticeFailed in src/main.ts). The card is mounted under the notice's own key, so that line belongs to the notice it was said about and to no other.
 */
function NoticeCard({ notice, now }: { notice: LiveNotice; now: Date }) {
  const dispatch = useAppDispatch();
  const [actOnNotice] = useActOnNoticeMutation();
  const [pressFailed, setPressFailed] = useState(false);
  // A question's card goes when its answer window does, the same moment the hover card takes its own down (armNotice in src/main.ts): past it the daemon has stopped waiting, so the buttons would do nothing.
  useEffect(() => {
    const at = Date.parse(notice.expires ?? "");
    if (Number.isNaN(at)) return;
    const timer = setTimeout(() => dispatch(ui.liveNoticeSet(undefined)), Math.max(0, at - Date.now()));
    return () => clearTimeout(timer);
  }, [notice.expires, dispatch]);
  const act = (action: string) => {
    setPressFailed(false);
    void actOnNotice({ ...notice, action })
      .unwrap()
      .catch(() => setPressFailed(true));
  };
  return (
    <div role="group" aria-label="Notice from Ora" className="relative flex gap-2 rounded-lg border bg-card px-2.5 py-2 group-data-[collapsible=icon]:hidden">
      <Face state={notice.kind === "error" ? "refused" : "noticed"} className="mt-0.5 text-meta" />
      <div className="flex min-w-0 flex-1 flex-col gap-0.5">
        {/* The sender line. The cross sits in the same row rather than floating over the corner, so nothing has to be padded clear of it. */}
        <div className="flex items-baseline gap-2">
          <span className="text-meta font-medium text-foreground">Ora</span>
          <span className="ml-auto text-micro text-muted-foreground">{noticeAge(notice.at, now)}</span>
          <button
            type="button"
            aria-label={noticeLabel("Close", notice.title)}
            onClick={() => dispatch(ui.liveNoticeSet(undefined))}
            className="-mr-0.5 flex size-4 shrink-0 items-center justify-center rounded text-muted-foreground opacity-50 transition-opacity hover:bg-hover hover:opacity-100 focus-visible:opacity-100 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            <X className="size-3" />
          </button>
        </div>
        {/* The title was stored and never drawn, which left the body to say on its own what the card was about. */}
        <p className="text-ui text-foreground">{notice.title}</p>
        <p role="status" className="line-clamp-3 text-meta text-muted-foreground">
          {notice.body}
        </p>
        {/* The four words on their own tell a screen reader nothing about what is being done or snoozed, so each button's own label names the notice it belongs to. They wrap because the rail is narrow and a notice may name buttons of its own: "Not happening" and "Start recording" are wider than the four this rail has short words for, and in one row they ran off the edge. */}
        <div className="mt-1 flex flex-wrap gap-1">
          {noticeButtons(notice.actions).map(({ key, label }) => (
            <Button key={key} variant="outline" size="xs" aria-label={noticeLabel(label, notice.title)} onClick={() => act(key)}>
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
    </div>
  );
}

/** What the rail says between the search field and the chats: the live notice on its card while there is one, or the one-line status otherwise. Input: the live notice, the status line, and the moment the ages are read against. Output: whichever of the two there is, or nothing. */
function SidebarNotice({ live, notice, now }: { live?: LiveNotice; notice?: { text: string; kind: "info" | "error" }; now: Date }) {
  if (live) return <NoticeCard key={`${live.kind}-${live.id}`} notice={live} now={now} />;
  if (!notice) return null;
  // ui.notice carries its own kind now, so a failure reads red (text-destructive) and plain status reads the same muted foreground as the rest of the sidebar's secondary text. The face sits outside the role="status" paragraph so the line's own textContent — what other screens assert against — still reads as the notice's words alone.
  return (
    <div className="flex items-center gap-1.5 px-1 group-data-[collapsible=icon]:hidden">
      <Face state={notice.kind === "error" ? "refused" : "noticed"} />
      <p role="status" className={`text-meta ${notice.kind === "error" ? "text-destructive" : "text-muted-foreground"}`}>
        {notice.text}
      </p>
    </div>
  );
}

/** What stands where the chat list would be when there is nothing in it. Input: whether the daemon answered, how many chats it has in total, and what is typed in the search box. Output: the one line. */
function NoChats({ up, total, query }: { up: boolean; total: number; query: string }) {
  const line = !up ? "Not connected." : total ? `Nothing matches “${query}”.` : "No chats yet.";
  return <p className="px-4 py-8 text-ui text-muted-foreground group-data-[collapsible=icon]:hidden">{line}</p>;
}

/** The screens along the foot of the rail. Input: the screen showing now. Output: one button each, the one showing marked both to the eye and to a screen reader. */
function SidebarNav({ place }: { place: Place }) {
  const dispatch = useAppDispatch();
  return (
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
  );
}

export function AppSidebar() {
  const dispatch = useAppDispatch();
  const { place, conversationId, query, notice, liveNotice } = useAppSelector((s) => s.ui);
  const { data: convs = [], isFetching, isLoading, isError } = useConversationsQuery();
  const oraState = useOraState(!isError);
  const list = useRef<HTMLDivElement>(null);

  const now = new Date();
  const today = now.toDateString();
  const groups = useMemo(() => groupConversations(chatsShown(convs, query.chats), new Date(today)), [convs, query.chats, today]);
  useFollowSelection(list, conversationId);

  /** Opens an unsaved draft rather than a conversation the daemon has to be told to make: nothing is posted and nothing appears in the list until the draft's own first message opens a real one, the same way a noticed task's first question does. */
  const newChat = () => dispatch(ui.chatDraftOpened());

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader className="gap-2 p-2">
        {/* Ora itself, first: its face and what it is doing, so the rail opens on who is here rather than on a button. */}
        <div className="flex items-center gap-2.5 px-2 pt-1 pb-2 group-data-[collapsible=icon]:hidden">
          <Face state={oraState} />
          <div className="flex min-w-0 flex-col">
            <span className="text-ui font-medium">ora</span>
            <span className="text-micro text-muted-foreground">{oraState}</span>
          </div>
        </div>
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
        <RunningNow />
        <SidebarNotice live={liveNotice} notice={notice} now={now} />
      </SidebarHeader>

      <SidebarContent>
        {/* Radix wraps what a viewport holds in a table box sized to its widest row, which is wider than the rail whenever a subtitle runs long; the row then clips at the rail's own edge instead of at the truncated span's, so no ellipsis ever shows. Pinning that box's own width to zero and its minimum width to the full rail (the same box the comment in parts.tsx's Scroller names) keeps it from growing past the rail, so the too-long row is what overflows and truncates, not the box around it. */}
        <ScrollArea ref={list} className="h-full [&>[data-radix-scroll-area-viewport]>div]:table-fixed [&>[data-radix-scroll-area-viewport]>div]:w-0 [&>[data-radix-scroll-area-viewport]>div]:min-w-full">
          {isLoading ? <ChatListSkeleton /> : null}
          {groups.map((g) => (
            <ChatGroup key={g.label} group={g} conversationId={conversationId} onChats={place === "chats"} now={now} />
          ))}
          {!isFetching && groups.length === 0 ? <NoChats up={!isError} total={convs.length} query={query.chats} /> : null}
        </ScrollArea>
      </SidebarContent>

      <SidebarFooter className="gap-0 border-t p-2">
        <SidebarNav place={place} />
      </SidebarFooter>
    </Sidebar>
  );
}
