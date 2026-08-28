package clock

import "testing"

// TestWeatherIsPure is the acceptance case: identical arguments give
// identical output — called twice, interleaved, in any order. The no-DB
// import guarantee is pinned separately in imports_test.go; purity of the
// value itself is what this file can prove in-process.
func TestWeatherIsPure(t *testing.T) {
	args := []struct {
		seed    string
		day     int64
		season  string
		climate string
	}{
		{"campaign-1", 0, "spring", ""},
		{"campaign-1", 365, "winter", ""},
		{"campaign-1", 365, "winter", ClimateDesert},
		{"other-seed", 365, "winter", ""},
		{"campaign-1", 7, "", ""},
		{"campaign-1", 8, "", ClimateArctic},
		{"campaign-1", 9, "summer", ClimateTropical},
	}
	first := make([]Forecast, len(args))
	for i, a := range args {
		first[i] = Weather(a.seed, a.day, a.season, a.climate)
	}
	// A second pass in reverse order must land on the same values.
	for i := len(args) - 1; i >= 0; i-- {
		a := args[i]
		if got := Weather(a.seed, a.day, a.season, a.climate); got != first[i] {
			t.Fatalf("Weather(%q,%d,%q,%q) changed between calls: %v then %v",
				a.seed, a.day, a.season, a.climate, first[i], got)
		}
	}
	// The seed is the re-roll: changing it must change at least one day over
	// a span, or the seed would be meaningless.
	differs := false
	for day := int64(0); day < 60 && !differs; day++ {
		if Weather("seed-a", day, "summer", "") != Weather("seed-b", day, "summer", "") {
			differs = true
		}
	}
	if !differs {
		t.Fatal("two different seeds produced a whole season of identical weather")
	}
}

// TestWeatherCoversSeasonsAndClimates is a smoke pass: every season ×
// climate combination produces a non-empty, table-sourced answer over a span
// of days, and climates actually bend the result.
func TestWeatherCoversSeasonsAndClimates(t *testing.T) {
	for _, season := range []string{"spring", "summer", "autumn", "winter", ""} {
		for _, climate := range []string{"", ClimateDesert, ClimateTropical, ClimateArctic, "unknown-climate"} {
			for day := int64(0); day < 30; day++ {
				w := Weather("seed", day, season, climate)
				if w.Summary == "" || w.Wind == "" {
					t.Fatalf("empty weather for season=%q climate=%q day=%d: %+v", season, climate, day, w)
				}
			}
		}
	}
	// The same day and season under different climates differ somewhere.
	anyDiff := false
	for day := int64(0); day < 90 && !anyDiff; day++ {
		a := Weather("seed", day, "summer", "")
		b := Weather("seed", day, "summer", ClimateArctic)
		anyDiff = a != b
	}
	if !anyDiff {
		t.Fatal("arctic climate never changed a summer day")
	}
}
