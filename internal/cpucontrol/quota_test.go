package cpucontrol

import "testing"

func TestValidateMillicoresRejectsZeroAndOversized(t *testing.T) {
	if err := ValidateMillicores(0); err == nil {
		t.Fatal("expected zero millicores to be rejected")
	}
	if err := ValidateMillicores(MaxMillicores + 1); err == nil {
		t.Fatal("expected an oversized value to be rejected")
	}
	if err := ValidateMillicores(1000); err != nil {
		t.Fatalf("expected 1000m to be valid: %v", err)
	}
}

func TestQuotaMicrosRoundTrip(t *testing.T) {
	cases := []uint64{1, 250, 1000, 4000, 999}
	for _, millicores := range cases {
		quota := QuotaMicros(millicores, DefaultPeriodMicros)
		back := MillicoresFromQuota(quota, DefaultPeriodMicros)
		if back != millicores {
			t.Fatalf("round trip for %dm: got %dm via quota %d", millicores, back, quota)
		}
	}
}

func TestQuotaMicrosOneCoreEqualsFullPeriod(t *testing.T) {
	if got := QuotaMicros(MillicoresPerCore, DefaultPeriodMicros); got != DefaultPeriodMicros {
		t.Fatalf("1000m should equal the full period (100%% of one core), got %d", got)
	}
}

func TestDescribeExpressesWholeServerPercentage(t *testing.T) {
	got := Describe(1000, 4)
	want := "1.00 core(s) (1000m) — about 25% of this 4-vCPU server"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestDescribeHandlesUnknownTotalCores(t *testing.T) {
	got := Describe(500, 0)
	if got != "0.50 core(s) (500m)" {
		t.Fatalf("got %q", got)
	}
}
