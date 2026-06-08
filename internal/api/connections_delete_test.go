package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codeintel/internal/auth"
	"codeintel/internal/db"
)

type deleteConnSpy struct {
	fakeAuthQuerier
	deleteCalls []deleteConnCall
	deleteErr   error
}
type deleteConnCall struct {
	OrgID        int32
	ConnectionID int32
}

func (d *deleteConnSpy) DeleteOrgConnection(ctx context.Context, orgID int32, connectionID int32) error {
	d.deleteCalls = append(d.deleteCalls, deleteConnCall{OrgID: orgID, ConnectionID: connectionID})
	return d.deleteErr
}

func newDeleteConnServer(spy *deleteConnSpy) *Server {
	return NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
	})
}

// TestDeleteConn_OwnerHappy_Returns200WithSuccessJSON locks the
// canonical happy path.
func TestDeleteConn_OwnerHappy_Returns200WithSuccessJSON(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &deleteConnSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}}
	srv := newDeleteConnServer(spy)

	req := httptest.NewRequest(http.MethodDelete, "/api/connections/42", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"success":true}` {
		t.Fatalf("body: got %q, want {\"success\":true}", got)
	}
	if len(spy.deleteCalls) != 1 {
		t.Fatalf("expected 1 DELETE call, got %d", len(spy.deleteCalls))
	}
	if spy.deleteCalls[0].OrgID != 7 || spy.deleteCalls[0].ConnectionID != 42 {
		t.Errorf("DELETE args: got (%d, %d), want (7, 42)", spy.deleteCalls[0].OrgID, spy.deleteCalls[0].ConnectionID)
	}
}

// TestDeleteConn_NotFound_Returns404 confirms the sentinel
// produces a 404 + the canonical "Connection not found." message.
func TestDeleteConn_NotFound_Returns404(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &deleteConnSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		deleteErr:       db.ErrConnectionNotFound,
	}
	srv := newDeleteConnServer(spy)

	req := httptest.NewRequest(http.MethodDelete, "/api/connections/9999", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
	want := `{"statusCode":404,"errorCode":"NOT_FOUND","message":"Connection not found."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
}

// TestDeleteConn_NonIntegerID_Returns400 covers the path-segment
// validation.
func TestDeleteConn_NonIntegerID_Returns400(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &deleteConnSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}}
	srv := newDeleteConnServer(spy)

	cases := []string{"abc", "1.5", "-not-int", "9999999999999999999999"}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			spy.deleteCalls = nil
			req := httptest.NewRequest(http.MethodDelete, "/api/connections/"+c, nil)
			req.Header.Set("X-Api-Key", "cik_"+secret)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("input %q: status %d, want 400", c, rec.Code)
			}
			if len(spy.deleteCalls) != 0 {
				t.Errorf("input %q: DELETE must not be called", c)
			}
		})
	}
}

// TestDeleteConn_MemberRole_Returns403 covers the OWNER guard byte
// for byte.
func TestDeleteConn_MemberRole_Returns403(t *testing.T) {
	const secret = "membersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	lookup := validOwnerLookup(hash)
	lookup.Role = "MEMBER"
	spy := &deleteConnSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: lookup}}
	srv := newDeleteConnServer(spy)
	req := httptest.NewRequest(http.MethodDelete, "/api/connections/42", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
	want := `{"statusCode":403,"errorCode":"INSUFFICIENT_PERMISSIONS","message":"Only organization owners can delete code host connections."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
}

// TestDeleteConn_NoCredentials_Returns401 covers the 401 path.
func TestDeleteConn_NoCredentials_Returns401(t *testing.T) {
	spy := &deleteConnSpy{}
	srv := newDeleteConnServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/connections/42", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestDeleteConn_DBError_Returns500WithoutLeak covers an outage.
func TestDeleteConn_DBError_Returns500WithoutLeak(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &deleteConnSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		deleteErr:       errors.New("simulated db outage"),
	}
	srv := newDeleteConnServer(spy)
	req := httptest.NewRequest(http.MethodDelete, "/api/connections/42", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "simulated db outage") {
		t.Errorf("raw db error leaked: %s", rec.Body.String())
	}
}
