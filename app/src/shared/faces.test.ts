import { describe, expect, it } from "vitest";

import { FACES, face, type OraState } from "./faces";

// Three sets stacked, newest first, because he wanted his picks to lead and the earlier ones kept: the faces he chose off the picker on 2026-09-12, then the minimal set drawn from that day's design sheets, then the hand-picked set of 2026-09-07. Nothing was thrown away; face() alternates through each state's list, so every face still gets its turn.
describe("Ora's faces", () => {
  it("leads with the picked face and keeps the earlier ones behind it", () => {
    expect(FACES.asleep).toEqual(["(-‿-)", "(￣o￣) zzZ"]);
    expect(FACES.watching).toEqual(["( •_•)>", "(•‿•)", "(・ω・)", "(¬‿¬)"]);
    expect(FACES.thinking).toEqual(["(￢_￢)…", "(-.-)", "(ノ_<。)", "( ・・)?", "(￣ω￣;)"]);
    expect(FACES.speaking).toEqual(["(-‿^)", "(^▽^)"]);
    expect(FACES.done).toEqual(["(•‿•)b", "(^‿^)", "(^-^)b"]);
    expect(FACES.noticed).toEqual(["(-‿-)!", "( ‘o’)"]);
    expect(FACES.listening).toEqual(["(•_•)", "( ･ω･)ﾉ", "(°ロ°)"]);
    expect(FACES.refused).toEqual(["(・_・;)", "(-_-)", "(￣ヘ￣)", "(ಠ_ಠ)"]);
    expect(FACES.dreaming).toEqual(["(-, -)…", "(˘‿˘)", "(￣.￣)…", "(っ˘ω˘ς )"]);
    expect(FACES.recording).toEqual(["(≖‿≖)", "(●_●)", "( ✧≖ ͜ʖ≖)"]);
  });

  // Stacking three sets is exactly how the same face ends up in one list twice, and a duplicate shows as the alternation stalling on one face for two turns rather than as anything a reader would call a bug.
  it("never holds the same face twice in one state", () => {
    for (const [state, faces] of Object.entries(FACES)) {
      expect(new Set(faces).size, `${state} repeats a face: ${faces.join(" ")}`).toBe(faces.length);
    }
  });

  it("alternates through a state's faces by tick and wraps", () => {
    expect(face("thinking", 0)).toBe("(￢_￢)…");
    expect(face("thinking", 4)).toBe("(￣ω￣;)");
    expect(face("thinking", 5)).toBe("(￢_￢)…");
    expect(face("speaking", 3)).toBe("(^▽^)");
  });

  it("has no state without a face", () => {
    for (const state of Object.keys(FACES) as OraState[]) expect(FACES[state].length).toBeGreaterThan(0);
  });

});
