package model

import (
	"time"

	// The IANA zone database, compiled into every binary that links this
	// package. Without it LoadLocation below answers whichever zones the
	// host filesystem happens to carry under /usr/share/zoneinfo, which
	// is a thing a container image can be built without -- and a
	// deployment whose wall clock silently fell back to UTC because its
	// base image was slimmed is exactly the failure this setting exists
	// to end. Roughly half a megabyte of binary, paid once, for a clock
	// that reads the same everywhere grain runs.
	_ "time/tzdata"
)

// DefaultTimeZone is the wall clock a deployment that has never chosen
// one keeps: US Pacific, which is where this deployment's operator
// actually is (grain/task-368). It is an IANA name rather than a fixed
// offset on purpose -- "America/Los_Angeles" is PST for part of the year
// and PDT for the rest, and a schedule set for 09:00 should stay at 09:00
// across the day that changes rather than sliding an hour.
//
// Every place that has to answer "what time is it here" with no stored
// setting to read starts from this one constant: DefaultConfig, the
// grain_config DDL and the migration that adds the column to a database
// predating it (schema.go, store.go), cmd/grain daemon's own -time-zone
// flag default, and the two API responses that describe a deployment
// with no config row yet (ui.GetSettings, ui's handleConfig).
const DefaultTimeZone = "America/Los_Angeles"

// TimeZoneOrDefault is name, or DefaultTimeZone when name is empty --
// the reading of "" that every consumer of Config.TimeZone shares. Empty
// is what a row written before the column existed, or by a test building
// a bare model.Config, reads back as, and there is no deployment that
// keeps no wall clock at all, so "" means "nobody has chosen" rather
// than naming a zone of its own.
//
// It does not check that the result names a real zone: LoadLocation does
// that, and does it in the one place it can do something useful about a
// name that turns out not to.
func TimeZoneOrDefault(name string) string {
	if name == "" {
		return DefaultTimeZone
	}
	return name
}

// LoadLocation resolves a stored zone name to the *time.Location every
// wall-clock computation in grain is done against -- Recurrence.Next
// chief among them, which is what decides when a daily, weekly or
// monthly schedule actually fires.
//
// Unlike time.LoadLocation it cannot fail. A name nothing answers to
// falls back to DefaultTimeZone, and that failing (which the embedded
// tzdata above makes impossible short of a corrupt build) falls back to
// UTC. A settings save is where a bad name is refused, while whoever
// typed it is still looking at it (ui.UpdateSettings, over
// ValidTimeZone); by the time a reconcile cycle is reading the row, the
// only useful answer to a name that no longer resolves is to keep firing
// schedules against some defensible clock rather than to stop.
func LoadLocation(name string) *time.Location {
	if loc, err := time.LoadLocation(TimeZoneOrDefault(name)); err == nil {
		return loc
	}
	if loc, err := time.LoadLocation(DefaultTimeZone); err == nil {
		return loc
	}
	return time.UTC
}

// ValidTimeZone reports whether name is an IANA zone this build can
// actually resolve -- ui.UpdateSettings' own check, so a typo is a
// validation error on the settings pane rather than a deployment that
// quietly keeps a different clock than the one on screen.
//
// "" is valid: it is how "leave it at the default" arrives from a client
// that has cleared the field, and TimeZoneOrDefault already gives it a
// meaning. "Local", which time.LoadLocation accepts, is not -- it names
// whatever zone the daemon's own host or TZ environment variable happens
// to be set to, which is precisely the accidental answer this setting
// replaces.
func ValidTimeZone(name string) bool {
	if name == "" {
		return true
	}
	if name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// Location is this deployment's wall clock as a *time.Location -- the
// zone Config.TimeZone names, resolved the forgiving way LoadLocation
// documents.
func (c Config) Location() *time.Location {
	return LoadLocation(c.TimeZone)
}
