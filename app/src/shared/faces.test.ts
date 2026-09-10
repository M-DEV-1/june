import { describe, expect, it } from "vitest";

import { FACES, face, type OraState } from "./faces";

// The set picked on 2026-09-07 from the faces sheet, by letter per row; a state with more than one face alternates through them in this order.
describe("Ora's faces", () => {
  it("holds the picked face for every state, in the picked order", () => {
    expect(FACES.watching).toEqual(["(・ω・)", "(¬‿¬)"]);
    expect(FACES.listening).toEqual(["( ･ω･)ﾉ", "(°ロ°)"]);
    expect(FACES.thinking).toEqual(["(ノ_<。)", "( ・・)?", "(￣ω￣;)"]);
    expect(FACES.speaking).toEqual(["(^▽^)"]);
    expect(FACES.done).toEqual(["(^-^)b"]);
    expect(FACES.refused).toEqual(["(￣ヘ￣)", "(ಠ_ಠ)"]);
    expect(FACES.asleep).toEqual(["(￣o￣) zzZ"]);
    expect(FACES.dreaming).toEqual(["(￣.￣)…", "(っ˘ω˘ς )"]);
    expect(FACES.noticed).toEqual(["( ‘o’)"]);
    expect(FACES.recording).toEqual(["( ✧≖ ͜ʖ≖)"]);
  });

  it("alternates through a state's faces by tick and wraps", () => {
    expect(face("thinking", 0)).toBe("(ノ_<。)");
    expect(face("thinking", 2)).toBe("(￣ω￣;)");
    expect(face("thinking", 3)).toBe("(ノ_<。)");
    expect(face("done", 7)).toBe("(^-^)b");
  });

  it("has no state without a face", () => {
    for (const state of Object.keys(FACES) as OraState[]) expect(FACES[state].length).toBeGreaterThan(0);
  });
});
