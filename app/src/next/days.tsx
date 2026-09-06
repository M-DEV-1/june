/** The Days screen: one day's page at a time — the sentences Ora wrote that night, under the day's own date and the one line the daemon writes about what the day held. The day is chosen from the picker in the header, which carries a search and each day's counts; there is no list of days beside the sidebar, because the sidebar is for chats. */

import { useDayQuery, useDaysQuery } from "./api";
import { activeDays, dayCounts, dayRailed, dayShort, daysShown, groupDays } from "./format";
import { Blank, HEAD, PageHeader, Picker, Scroller, TAIL, useWide } from "./parts";
import { ui, useAppDispatch, useAppSelector } from "./store";
import { Page } from "./day-page";

/** The Days screen. Input: none. Output: the header with the day picker in it and the chosen day's page under it. Today is in the list before the nightly loop has written its page, so the window opens on the newest day that actually has one. */
export function DaysScreen() {
  const dispatch = useAppDispatch();
  const { date, query } = useAppSelector((s) => s.ui);
  const { data: days = [], isError } = useDaysQuery();
  const [wide, pane] = useWide();

  const listed = daysShown(activeDays(days), query.days);
  const chosen = date ?? (days.find((d) => d.has_page) ?? days[0])?.date;
  // currentData rather than data: RTK Query keeps the previous arg's result in data while the new day's own fetch is in flight, which would show the day just left under the date just picked.
  const { currentData: page } = useDayQuery(chosen ?? "", { skip: !chosen });

  // Each row carries what the day holds: the counts the daemon reported, and for a day the nightly loop has not written yet, that it has no page.
  const options = groupDays(listed).flatMap((g) => g.items.map((d) => ({ id: d.date, label: dayShort(d.date), hint: dayCounts(d) || (d.has_page ? "" : "no page yet"), group: g.label })));

  return (
    <div ref={pane} data-pane className="flex h-full min-h-0 flex-col">
      <PageHeader wide={wide} railed={Boolean(page && dayRailed(page))}>
        <h1 className="shrink-0 text-ui font-medium">Days</h1>
        <Picker
          list="days"
          label="Choose a day"
          placeholder="Search days"
          options={options}
          selected={chosen}
          empty={activeDays(days).length ? `Nothing matches “${query.days}”.` : "No days written yet."}
          onPick={(id) => dispatch(ui.dayOpened(id))}
        />
      </PageHeader>
      <Scroller bodyClassName={page ? `${HEAD} ${TAIL}` : "flex"}>
        {page ? (
          <Page page={page} wide={wide} />
        ) : (
          <Blank
            up={!isError}
            empty="No days written yet."
            hint="Ora writes a page for the day each night, from what it saw and heard. The first one appears after the first full day it has been running."
          />
        )}
      </Scroller>
    </div>
  );
}
