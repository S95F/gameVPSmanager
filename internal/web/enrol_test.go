package web

// The enrolment page: a single-use URL, the visitor's own password and TOTP,
// and one sentence for every kind of dead invite.

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var secretRE = regexp.MustCompile(`secret=([A-Z2-7]+)`)

func TestEnrolHappyPath(t *testing.T) {
	id := &stubIdentity{users: 1, code: "123456"}
	h := loginServer(t, id)

	// GET shows the form for the invite's email, with a generated secret.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/enrol?token=gvmi_good", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /enrol = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "friend@example.com") || !strings.Contains(body, "otpauth://totp/gvm:") {
		t.Fatalf("form should name the invitee and carry the otpauth line:\n%s", body)
	}
	m := secretRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no TOTP secret on the form")
	}

	// A second GET shows the SAME secret (the authenticator was already fed).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/enrol?token=gvmi_good", nil))
	if !strings.Contains(rec.Body.String(), m[1]) {
		t.Error("a reload must keep the already-shown secret")
	}

	// POST with matching passwords and the right code enrols and lands on
	// the login page.
	rec = postForm(h, "/enrol", map[string]string{
		"token": "gvmi_good", "password": "correct horse battery staple",
		"password2": "correct horse battery staple", "code": "123456",
	}, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("enrol POST: %d → %q; body: %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if !id.enrolled {
		t.Error("the stub was never asked to enrol")
	}
}

func TestEnrolDeadInviteOneSentence(t *testing.T) {
	h := loginServer(t, &stubIdentity{users: 1})
	for _, target := range []string{"/enrol", "/enrol?token=gvmi_wrong"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusGone {
			t.Errorf("%s = %d, want 404/410", target, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "no longer valid") {
			t.Errorf("%s should carry the one dead-invite sentence", target)
		}
		if strings.Contains(rec.Body.String(), "<form") {
			t.Errorf("%s must render no form", target)
		}
	}
}

func TestEnrolValidations(t *testing.T) {
	id := &stubIdentity{users: 1, code: "123456"}
	h := loginServer(t, id)

	// Prime the secret.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/enrol?token=gvmi_good", nil))

	// Short password.
	rec := postForm(h, "/enrol", map[string]string{
		"token": "gvmi_good", "password": "short", "password2": "short", "code": "123456",
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "at least") {
		t.Errorf("short password: %d", rec.Code)
	}

	// Mismatch.
	rec = postForm(h, "/enrol", map[string]string{
		"token": "gvmi_good", "password": "correct horse battery", "password2": "wrong horse", "code": "123456",
	}, nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "do not match") {
		t.Errorf("mismatch: %d", rec.Code)
	}

	// Wrong code: re-rendered with the SAME secret, invite not consumed.
	rec = postForm(h, "/enrol", map[string]string{
		"token": "gvmi_good", "password": "correct horse battery staple",
		"password2": "correct horse battery staple", "code": "000000",
	}, nil)
	if rec.Code != http.StatusUnauthorized || id.enrolled {
		t.Errorf("wrong code: %d enrolled=%v", rec.Code, id.enrolled)
	}
}
