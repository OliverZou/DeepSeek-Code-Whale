package commands

import (
	"testing"
	"time"
)

func TestParseTeamCommand(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		line     string
		goal     string
		teamName string
		list     bool
		wantErr  bool
	}{
		{line: "/team", list: true},
		{line: "/team list", list: true},
		{line: "/team 开发登录页", goal: "开发登录页"},
		{line: "/team 做一件事 --team demo", goal: "做一件事", teamName: "demo"},
		{line: "/team --team demo", wantErr: true},
	}
	for _, tc := range cases {
		res, err := Parse(tc.line, "sid", now)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: expected error", tc.line)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error: %v", tc.line, err)
			continue
		}
		if !res.Handled {
			t.Errorf("%q: not handled", tc.line)
			continue
		}
		if res.TeamGoal != tc.goal {
			t.Errorf("%q: TeamGoal = %q, want %q", tc.line, res.TeamGoal, tc.goal)
		}
		if res.TeamName != tc.teamName {
			t.Errorf("%q: TeamName = %q, want %q", tc.line, res.TeamName, tc.teamName)
		}
		if res.TeamList != tc.list {
			t.Errorf("%q: TeamList = %v, want %v", tc.line, res.TeamList, tc.list)
		}
	}
}
