package model

import (
	"errors"
	"fmt"
	"testing"
)

// Dolt's own wording, captured from a real embedded engine by
// pkg/model/dolt's TestDoltReportsASerializationFailure, which asserts
// the engine still says this. The two fail together if Dolt rewords it.
const doltSerializationFailure = "Error 1213: serialization failure: this transaction " +
	"conflicts with a committed transaction from another client, try restarting transaction."

func TestIsSerializationFailureRecognisesALostRace(t *testing.T) {
	for name, err := range map[string]error{
		"dolt":                    errors.New(doltSerializationFailure),
		"wrapped by a caller":     fmt.Errorf("writing task 7: %w", errors.New(doltSerializationFailure)),
		"mysql deadlock":          errors.New("Error 1213: Deadlock found when trying to get lock; try restarting transaction"),
		"mysql lock wait timeout": errors.New("Error 1205: Lock wait timeout exceeded; try restarting transaction"),
	} {
		t.Run(name, func(t *testing.T) {
			if !isSerializationFailure(err) {
				t.Fatalf("%v was not recognised, so write would surface it instead of retrying", err)
			}
		})
	}
}

func TestIsSerializationFailureLeavesEverythingElseAlone(t *testing.T) {
	for name, err := range map[string]error{
		"nil":              nil,
		"connection error": errors.New("connection refused"),
		"unknown column":   errors.New("Error 1054: Unknown column `nope` in field list"),
		"caller's own":     errors.New("updating task 7: no such task"),
	} {
		t.Run(name, func(t *testing.T) {
			if isSerializationFailure(err) {
				t.Fatalf("%v was misread as a lost race, so write would retry it pointlessly", err)
			}
		})
	}
}
