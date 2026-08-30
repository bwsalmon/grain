package ui_test

// The /api/templates HTTP surface (bwsalmon/agents#516) --
// server_schedules_test.go's own doc comment on why this only asserts
// status codes and JSON shape, leaving Client's own behaviour to
// templates_test.go.

import (
	"net/http"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/ui"
)

func TestTemplatesCreateListUpdateDelete(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, http.MethodPost, "/api/templates",
		`{"name":"Dependency bump","title":"Bump dependencies","repo":"acme/widgets"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body)
	}
	created := decode[ui.Template](t, rec)
	if created.ID == "" {
		t.Fatal("created template came back with no id")
	}

	rec = do(t, srv, http.MethodGet, "/api/templates", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", rec.Code)
	}
	list := decode[[]ui.Template](t, rec)
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v, want just %s", list, created.ID)
	}

	rec = do(t, srv, http.MethodPatch, "/api/templates/"+created.ID, `{"title":"Bump dependencies (patch only)"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200: %s", rec.Code, rec.Body)
	}
	updated := decode[ui.Template](t, rec)
	if updated.Title != "Bump dependencies (patch only)" {
		t.Errorf("title = %q, want it updated", updated.Title)
	}

	rec = do(t, srv, http.MethodDelete, "/api/templates/"+created.ID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204: %s", rec.Code, rec.Body)
	}

	rec = do(t, srv, http.MethodGet, "/api/templates", "")
	list = decode[[]ui.Template](t, rec)
	if len(list) != 0 {
		t.Fatalf("list after delete = %+v, want empty", list)
	}
}

func TestTemplatesCreateRejectsAMissingNameWith400(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodPost, "/api/templates", `{"title":"x","repo":"acme/widgets"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestTemplatesUpdateOnAnUnknownIDIs404(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodPatch, "/api/templates/nope", `{"title":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

func TestTemplatesDeleteOnAnUnknownIDIs404(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodDelete, "/api/templates/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

// TestScheduleCreatedFromATemplateOverHTTP is a thin end-to-end check
// that the /api/schedules and /api/templates surfaces compose: a
// schedule created over HTTP naming templateId comes back carrying that
// template's content, the same as ui.Client.CreateSchedule already
// checks directly (schedules_test.go).
func TestScheduleCreatedFromATemplateOverHTTP(t *testing.T) {
	srv, _ := testServer(t)
	rec := do(t, srv, http.MethodPost, "/api/templates",
		`{"name":"Dependency bump","title":"Bump dependencies","repo":"acme/widgets"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create template status = %d, want 201: %s", rec.Code, rec.Body)
	}
	tmpl := decode[ui.Template](t, rec)

	rec = do(t, srv, http.MethodPost, "/api/schedules",
		`{"templateId":"`+tmpl.ID+`","recurrence":{"kind":"everyNHours","everyNHours":24}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create schedule status = %d, want 201: %s", rec.Code, rec.Body)
	}
	sched := decode[ui.Schedule](t, rec)
	if sched.TemplateID != tmpl.ID {
		t.Errorf("templateId = %q, want %q", sched.TemplateID, tmpl.ID)
	}
	if sched.Title != "Bump dependencies" || sched.Repo != "acme/widgets" {
		t.Errorf("title/repo = %q/%q, want the template's own", sched.Title, sched.Repo)
	}
}
