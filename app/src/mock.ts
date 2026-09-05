/** Seed data for the window: the venue matter's script, the CI-red matter already answered, and the lease stub. */
import type { Evidence, Matter, View } from "./state";

/** The one email both venue turns read from. Turn one leaves it collapsed; turn two is the same source, expanded. */
const venueEvidence: Evidence = {
  title: "Re: venue for the 12th",
  meta: "Priya Nair · 08:40",
  body:
    "Hi, Tuesday works for us. <mark>Could you send the invoice across first?</mark> Once it's in I'll confirm the room with the building. <mark>Parking is tight after 6</mark>, just so you know. Priya",
};

/** The answer and evidence for each successive question asked in the venue matter. */
export const venueScript: { a: string; evidence?: Evidence[] }[] = [
  {
    a: "Nearly. Priya said <b>yes for Tuesday</b>, but she wants the invoice before she confirms the room. I have it drafted from last month's.",
    evidence: [venueEvidence],
  },
  {
    a: 'Only that <b>parking is tight after 6</b>. Nothing to act on.',
    evidence: [venueEvidence],
  },
];

function initialMatters(): Matter[] {
  return [
    { id: "venue", title: "venue", context: "Mail · Priya Nair", turns: [] },
    {
      id: "ci",
      title: "CI red",
      context: "GitHub · 08:12",
      turns: [
        {
          q: "CI red on main",
          a: "<b>test_upload</b> fails on the new size check: it expects 10 MB and the constant now says 8. Sam lowered it in #412 yesterday.",
          evidence: [{ title: "CI run 4471", meta: "GitHub · 08:12" }],
        },
      ],
    },
    { id: "lease", title: "lease", context: "Mail · Anita", turns: [] },
  ];
}

/** The window as it looks the moment it opens: nothing asked yet, three matters held. */
export function initialView(): View {
  return {
    matters: initialMatters(),
    current: 0,
    evidenceOpen: false,
    input: "",
    state: "empty",
    contextChip: "",
    contextText: "",
  };
}
