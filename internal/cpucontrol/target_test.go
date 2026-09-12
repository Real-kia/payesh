package cpucontrol

import "testing"

func TestDistinctTargetNamesCannotAliasCgroupPath(t *testing.T) {
	a := Target{Kind: TargetKindService, Name: "worker:a"}.GroupPath()
	b := Target{Kind: TargetKindService, Name: "worker_a"}.GroupPath()
	if a == b {
		t.Fatalf("distinct targets aliased %q", a)
	}
}
