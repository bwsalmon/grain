package ui_test

// The /api/schedules HTTP surface -- server_test.go's own doc comment on
// why this only asserts status codes and JSON shape, leaving Client's own
// behaviour to schedules_test.go.

import (
	"net/http"
	"testing"

	"github.com/bwsalmon/grain/pkg/ui"
)

func TestSchedulesCreateListUpdateDelete(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodPost, "/api/schedules",
		`{"title":"nightly bump","repo":"acme/widgets","recurrence":{"kind":"everyNHours","everyNHours":24}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body)
	}
	created := decode[ui.Schedule](t, rec)
	if created.ID == "" {
		t.Fatal("created schedule came back with no id")
	}
	if !created.Enabled {
		t.Error("want a freshly created schedule enabled")
	}

	rec = do(t, srv, http.MethodGet, "/api/schedules", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", rec.Code)
	}
	list := decode[[]ui.Schedule](t, rec)
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v, want just %s", list, created.ID)
	}

	rec = do(t, srv, http.MethodPatch, "/api/schedules/"+created.ID, `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200: %s", rec.Code, rec.Body)
	}
	updated := decode[ui.Schedule](t, rec)
	if updated.Enabled {
		t.Error("want the schedule paused after the update")
	}

	rec = do(t, srv, http.MethodDelete, "/api/schedules/"+created.ID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204: %s", rec.Code, rec.Body)
	}

	rec = do(t, srv, http.MethodGet, "/api/schedules", "")
	list = decode[[]ui.Schedule](t, rec)
	if len(list) != 0 {
		t.Fatalf("list after delete = %+v, want empty", list)
	}
}

func TestSchedulesCreateRejectsAMissingTitleWith400(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodPost, "/api/schedules",
		`{"repo":"acme/widgets","recurrence":{"kind":"everyNHours","everyNHours":24}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestSchedulesUpdateOnAnUnknownIDIs404(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodPatch, "/api/schedules/nope", `{"enabled":false}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

func TestSchedulesDeleteOnAnUnknownIDIs404(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodDelete, "/api/schedules/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

// The suite-backed half of the same surface: a schedule that runs a task
// suite goes through the very same four routes, with suiteId in place of
// the title and content fields (ui.CreateScheduleRequest's own doc
// comment on why they are mutually exclusive).
func TestSchedulesCreateFromASuite(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodPost, "/api/templates",
		`{"name":"Find bugs","title":"Find and fix a bug"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create template status = %d, want 201: %s", rec.Code, rec.Body)
	}
	tmpl := decode[ui.Template](t, rec)

	rec = do(t, srv, http.MethodPost, "/api/suites",
		`{"name":"Bug sweep","templateIds":["`+tmpl.ID+`"],"mode":"until_clean","maxPasses":5}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create suite status = %d, want 201: %s", rec.Code, rec.Body)
	}
	suite := decode[ui.Suite](t, rec)

	rec = do(t, srv, http.MethodPost, "/api/schedules",
		`{"suiteId":"`+suite.ID+`","repo":"acme/widgets","base":"main",`+
			`"recurrence":{"kind":"daily","timeOfDay":"09:00"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create schedule status = %d, want 201: %s", rec.Code, rec.Body)
	}
	created := decode[ui.Schedule](t, rec)
	if created.SuiteID != suite.ID || created.SuiteName != "Bug sweep" {
		t.Fatalf("created = %+v, want it naming the suite it runs", created)
	}

	// Deleting that suite while the schedule still runs it is a 400, not
	// a silent orphaning (ui.Client.DeleteSuite's own guard).
	rec = do(t, srv, http.MethodDelete, "/api/suites/"+suite.ID, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete-in-use status = %d, want 400: %s", rec.Code, rec.Body)
	}
}
