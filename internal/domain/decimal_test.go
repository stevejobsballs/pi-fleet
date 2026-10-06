package domain

import "testing"

func TestToleranceLimits(t *testing.T) {
	tests := []struct {
		name    string
		tol     Tolerance
		nominal string
		lo, hi  string
	}{
		{"absolute", Tolerance{Kind: TolAbsolute, Value: "0.05"}, "10.00", "9.95", "10.05"},
		{"percent", Tolerance{Kind: TolPercent, Value: "1"}, "250", "247.5", "252.5"},
		{"percent of negative nominal", Tolerance{Kind: TolPercent, Value: "2"}, "-50", "-51", "-49"},
		{"limits", Tolerance{Kind: TolLimits, Lower: "35.5", Upper: "37.5"}, "36.5", "35.5", "37.5"},
		{"no float error", Tolerance{Kind: TolAbsolute, Value: "0.1"}, "0.2", "0.1", "0.3"},
		{"repeating", Tolerance{Kind: TolPercent, Value: "1"}, "3.333", "3.29967", "3.36633"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lo, hi, err := tt.tol.Limits(tt.nominal)
			if err != nil {
				t.Fatal(err)
			}
			if formatLimit(lo) != tt.lo || formatLimit(hi) != tt.hi {
				t.Errorf("limits = [%s, %s], want [%s, %s]", formatLimit(lo), formatLimit(hi), tt.lo, tt.hi)
			}
		})
	}
}

func TestWithinIsInclusiveAndExact(t *testing.T) {
	lo, hi, err := Tolerance{Kind: TolAbsolute, Value: "0.1"}.Limits("0.2")
	if err != nil {
		t.Fatal(err)
	}
	// In float64, 0.2 + 0.1 > 0.3; exact arithmetic must accept 0.3.
	for reading, want := range map[string]bool{"0.3": true, "0.1": true, "0.30001": false, "0.09999": false, "0.25": true} {
		got, err := within(reading, lo, hi)
		if err != nil || got != want {
			t.Errorf("within(%s) = %v, %v; want %v", reading, got, err, want)
		}
	}
}

func TestToleranceRejects(t *testing.T) {
	for _, tc := range []struct {
		tol     Tolerance
		nominal string
	}{
		{Tolerance{Kind: TolAbsolute, Value: "-1"}, "10"},
		{Tolerance{Kind: TolAbsolute, Value: "1e-3"}, "10"},
		{Tolerance{Kind: TolAbsolute, Value: "1", Lower: "2"}, "10"},
		{Tolerance{Kind: TolLimits, Lower: "5", Upper: "4"}, "4.5"},
		{Tolerance{Kind: TolLimits, Lower: "4", Upper: "5", Value: "1"}, "4.5"},
		{Tolerance{Kind: "sigma", Value: "1"}, "10"},
		{Tolerance{Kind: TolPercent, Value: "1"}, "ten"},
		{Tolerance{Kind: TolAbsolute, Value: ".5"}, "10"},
	} {
		if _, _, err := tc.tol.Limits(tc.nominal); err == nil {
			t.Errorf("%+v with nominal %q accepted", tc.tol, tc.nominal)
		}
	}
}
