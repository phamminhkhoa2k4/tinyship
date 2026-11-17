package cli

// `tinyship app hibernate` tests: the request it sends, the flags it
// accepts, and the answer it prints. The API side has its own tests.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// hibernateAPI answers the toggle with a realistic app payload and
// records the request it received.
func hibernateAPI(t *testing.T, state string, enabled bool) func() (string, string, map[string]any) {
	t.Helper()
	var method, path string
	var body map[string]any
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		body = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, http.StatusOK, map[string]any{
			"name": "web", "hibernate_state": state,
			"config": map[string]any{
				"hibernate": map[string]any{
					"enabled": enabled, "idle_timeout_s": 600, "wake_timeout_s": 30,
				},
			},
		})
	}))
	return func() (string, string, map[string]any) { return method, path, body }
}

func TestAppHibernatePostsTheToggle(t *testing.T) {
	req := hibernateAPI(t, "awake", true)

	code, stdout, stderr := run(t, "app", "hibernate", "web", "--on")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	method, path, body := req()
	if method != http.MethodPost || path != "/apps/web/hibernate" {
		t.Errorf("request = %s %s, want POST /apps/web/hibernate", method, path)
	}
	if body["enabled"] != true {
		t.Errorf("body = %v, want enabled true", body)
	}
	if !strings.Contains(stdout, "hibernation on for web") {
		t.Errorf("stdout = %q, want the enabled summary", stdout)
	}
	if !strings.Contains(stdout, "600") || !strings.Contains(stdout, "30") {
		t.Errorf("stdout = %q, want the idle and wake windows", stdout)
	}
}

func TestAppHibernateWithNoFlagSendsAnEmptyBodyToInvert(t *testing.T) {
	req := hibernateAPI(t, "", false)

	code, stdout, stderr := run(t, "app", "hibernate", "web")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	_, _, body := req()
	if _, asked := body["enabled"]; asked {
		t.Errorf("body = %v, want no enabled field so the API inverts", body)
	}
	if !strings.Contains(stdout, "hibernation off for web") {
		t.Errorf("stdout = %q, want the disabled summary", stdout)
	}
}

func TestAppHibernateOffExplainsAStillSleepingApp(t *testing.T) {
	req := hibernateAPI(t, "sleeping", false)

	code, stdout, stderr := run(t, "app", "hibernate", "web", "--off")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	_, _, body := req()
	if body["enabled"] != false {
		t.Errorf("body = %v, want enabled false", body)
	}
	if !strings.Contains(stdout, "wakes on its next request") {
		t.Errorf("stdout = %q, want the wake note for a sleeping app", stdout)
	}
}

func TestAppHibernateRefusesBothFlags(t *testing.T) {
	code, _, stderr := run(t, "app", "hibernate", "web", "--on", "--off")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "exclusive") {
		t.Errorf("stderr = %q, want the exclusivity complaint", stderr)
	}
}

func TestAppHibernateRequiresExactlyOneName(t *testing.T) {
	if code, _, _ := run(t, "app", "hibernate"); code != 2 {
		t.Errorf("no name: exit = %d, want 2", code)
	}
	if code, _, _ := run(t, "app", "hibernate", "a", "b"); code != 2 {
		t.Errorf("two names: exit = %d, want 2", code)
	}
}

func TestAppHibernateSurfacesTheAPIError(t *testing.T) {
	apiFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]string{
				"code": "E_CONFLICT", "message": "app has an unreadable config"},
		})
	}))
	code, _, stderr := run(t, "app", "hibernate", "web", "--on")
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "E_CONFLICT") {
		t.Errorf("stderr = %q, want the API's code", stderr)
	}
}
