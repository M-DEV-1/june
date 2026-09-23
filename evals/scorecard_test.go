package main

import "testing"

// TestTrack1Rate_SkipsJudgeOutages pins the headline number against a spent Gemini minute: a question whose judge call failed was never measured, so counting it as a failed retrieval turns an outage into an apparent regression. Track 8 already excludes those rows; track 1 has to do the same.
func TestTrack1Rate_SkipsJudgeOutages(t *testing.T) {
	rs := []track1Result{
		{V: verdict{Verdict: "pass"}},
		{V: verdict{Verdict: "fail"}},
		{JudgeErr: "429 RESOURCE_EXHAUSTED", V: verdict{Verdict: "fail", Why: "judge call failed"}},
	}
	passes, total := track1Rate(rs)
	if passes != 1 || total != 2 {
		t.Fatalf("track1Rate = %d/%d, want 1/2: the judge outage row must not be counted", passes, total)
	}
	if got := track1Outages(rs); got != 1 {
		t.Fatalf("track1Outages = %d, want 1", got)
	}
}
