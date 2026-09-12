package main

import "testing"

func TestCompareExactLargeCounters(t *testing.T) {
	r, err := compare("90071992547409930", "90071992547409931", "0.0000012")
	if err != nil {
		t.Fatal(err)
	}
	if r.AbsoluteDelta != "1" || r.DeltaDirection != "payesh-lower" || !r.WithinTolerance {
		t.Fatalf("report=%+v", r)
	}
}

func TestCompareRejectsMismatchAndInvalidInput(t *testing.T) {
	r, err := compare("120", "100", "5")
	if err != nil || r.WithinTolerance || r.DeltaPercent != "20.000000" {
		t.Fatalf("report=%+v err=%v", r, err)
	}
	for _, input := range []string{"", "-1", "+1", "01", "1.5"} {
		if _, err := decimalUint(input); err == nil {
			t.Fatalf("invalid input accepted: %q", input)
		}
	}
}

func TestCompareHandlesZeroProvider(t *testing.T) {
	zero, err := compare("0", "0", "0")
	if err != nil || !zero.WithinTolerance {
		t.Fatalf("zero report=%+v err=%v", zero, err)
	}
	nonzero, err := compare("1", "0", "100")
	if err != nil || nonzero.WithinTolerance {
		t.Fatalf("nonzero report=%+v err=%v", nonzero, err)
	}
}
