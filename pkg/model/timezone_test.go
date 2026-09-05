package model_test

import (
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
)

// The zone database is compiled in (timezone.go's own import), so this
// answers the same way on a slimmed container image as on a developer's
// laptop -- which is the failure the embedded copy is there to prevent.
func TestLoadLocationResolvesTheDefaultZoneWithoutTheHostsZoneinfo(t *testing.T) {
	loc := model.LoadLocation(model.DefaultTimeZone)
	if loc.String() != model.DefaultTimeZone {
		t.Fatalf("LoadLocation(%q) = %q, want the zone itself", model.DefaultTimeZone, loc)
	}
	// Winter, so this is the standing offset rather than the summer one:
	// Pacific is eight hours behind UTC.
	_, offset := time.Date(2026, time.January, 15, 12, 0, 0, 0, loc).Zone()
	if want := -8 * 60 * 60; offset != want {
		t.Errorf("January offset = %ds, want %ds", offset, want)
	}
}

func TestLoadLocationFallsBackToTheDefaultForANameNothingAnswersTo(t *testing.T) {
	if got := model.LoadLocation("Mars/Olympus_Mons"); got.String() != model.DefaultTimeZone {
		t.Errorf("LoadLocation of a nonsense zone = %q, want %q", got, model.DefaultTimeZone)
	}
	if got := model.LoadLocation(""); got.String() != model.DefaultTimeZone {
		t.Errorf("LoadLocation(\"\") = %q, want %q", got, model.DefaultTimeZone)
	}
}

func TestTimeZoneOrDefaultOnlyFillsInAnEmptyName(t *testing.T) {
	if got := model.TimeZoneOrDefault(""); got != model.DefaultTimeZone {
		t.Errorf("TimeZoneOrDefault(\"\") = %q, want %q", got, model.DefaultTimeZone)
	}
	if got := model.TimeZoneOrDefault("UTC"); got != "UTC" {
		t.Errorf("TimeZoneOrDefault(\"UTC\") = %q, want it left alone", got)
	}
}

func TestValidTimeZone(t *testing.T) {
	for _, name := range []string{"", "UTC", "America/Los_Angeles", "Europe/Berlin"} {
		if !model.ValidTimeZone(name) {
			t.Errorf("ValidTimeZone(%q) = false, want true", name)
		}
	}
	// "Local" resolves for time.LoadLocation and is refused here on
	// purpose: it names whatever the daemon's host happens to be set to,
	// which is the accidental answer this setting replaces.
	for _, name := range []string{"Mars/Olympus_Mons", "Local", "PST", "-08:00"} {
		if model.ValidTimeZone(name) {
			t.Errorf("ValidTimeZone(%q) = true, want false", name)
		}
	}
}

// A deployment that has never chosen keeps Pacific time, rather than the
// UTC a container with no zone of its own would otherwise fall into.
func TestDefaultConfigKeepsPacificTime(t *testing.T) {
	cfg := model.DefaultConfig()
	if cfg.TimeZone != model.DefaultTimeZone {
		t.Fatalf("DefaultConfig().TimeZone = %q, want %q", cfg.TimeZone, model.DefaultTimeZone)
	}
	if got := cfg.Location().String(); got != model.DefaultTimeZone {
		t.Errorf("Location() = %q, want %q", got, model.DefaultTimeZone)
	}
	// And a config that stores nothing still resolves to it, which is
	// what a row written before the column existed reads back as.
	if got := (model.Config{}).Location().String(); got != model.DefaultTimeZone {
		t.Errorf("zero Config Location() = %q, want %q", got, model.DefaultTimeZone)
	}
}
