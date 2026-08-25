package team_engine

import (
	"os"
	"testing"
)

func TestRunIndexRecordResolve(t *testing.T) {
	old := os.Getenv("USERPROFILE")
	os.Setenv("USERPROFILE", t.TempDir())
	defer os.Setenv("USERPROFILE", old)

	RecordRunIndex("master-1", "D:\\ws\\proj", "goal 2048")
	wd, ok := ResolveRunIndex("master-1")
	if !ok || wd != "D:\\ws\\proj" {
		t.Fatalf("ResolveRunIndex = %q/%v, want D:\\ws\\proj/true", wd, ok)
	}
	if _, ok := ResolveRunIndex("master-unknown"); ok {
		t.Fatalf("unknown master must not resolve")
	}
}
