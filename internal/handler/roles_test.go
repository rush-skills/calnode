package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// PATCH /v1/users/{id}/role  (owner only)
// ---------------------------------------------------------------------------

func TestSetUserRole_ownerPromotesAndDemotes(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','m@example.com','Member','UTC',0)`)

	// Promote member → admin.
	req := authReq(http.MethodPatch, "/v1/users/u2/role", `{"role":"admin"}`, ownerKey)
	req.SetPathValue("id", "u2")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.SetUserRole)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("promote: got %d — %s", rec.Code, rec.Body.String())
	}
	var isAdmin int
	database.QueryRow(`SELECT is_admin FROM users WHERE id='u2'`).Scan(&isAdmin)
	if isAdmin != 1 {
		t.Error("u2 should be admin after promotion")
	}

	// Demote admin → member.
	req = authReq(http.MethodPatch, "/v1/users/u2/role", `{"role":"member"}`, ownerKey)
	req.SetPathValue("id", "u2")
	rec = httptest.NewRecorder()
	h.RequireAuth(h.SetUserRole)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("demote: got %d — %s", rec.Code, rec.Body.String())
	}
	database.QueryRow(`SELECT is_admin FROM users WHERE id='u2'`).Scan(&isAdmin)
	if isAdmin != 0 {
		t.Error("u2 should be member after demotion")
	}
}

// An admin who is not the owner can grant admin to a member, so a workspace is not
// bottlenecked on one person; taking admin away from another admin stays owner-only,
// the same line password resets and archiving draw.
func TestSetUserRole_adminCanPromoteButNotDemoteAdmin(t *testing.T) {
	h, database, _, _ := setupWorkspaceWithDB(t)
	adminKey := "admin-not-owner-key"
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin,is_owner) VALUES ('u2','a@example.com','Admin','UTC',1,0)`)
	database.Exec(`INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES ('k2','u2','t',?,'2024-01-01')`, sha256HexForTest(adminKey))
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','m@example.com','Member','UTC',0)`)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u4','b@example.com','Other admin','UTC',1)`)

	call := func(target, role string) *httptest.ResponseRecorder {
		req := authReq(http.MethodPatch, "/v1/users/"+target+"/role", `{"role":"`+role+`"}`, adminKey)
		req.SetPathValue("id", target)
		rec := httptest.NewRecorder()
		h.RequireAuth(h.SetUserRole)(rec, req)
		return rec
	}

	if rec := call("u3", "admin"); rec.Code != http.StatusOK {
		t.Errorf("admin promoting a member: got %d; want 200 — %s", rec.Code, rec.Body.String())
	}
	var isAdmin int
	database.QueryRow(`SELECT is_admin FROM users WHERE id = 'u3'`).Scan(&isAdmin)
	if isAdmin != 1 {
		t.Errorf("u3 is_admin = %d; want 1", isAdmin)
	}

	if rec := call("u4", "member"); rec.Code != http.StatusForbidden {
		t.Errorf("admin demoting another admin: got %d; want 403", rec.Code)
	}
	database.QueryRow(`SELECT is_admin FROM users WHERE id = 'u4'`).Scan(&isAdmin)
	if isAdmin != 1 {
		t.Errorf("u4 is_admin = %d; want 1 (unchanged)", isAdmin)
	}

	// A plain member still cannot touch roles at all.
	memberKey := "member-key"
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u5','c@example.com','Member2','UTC',0)`)
	database.Exec(`INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES ('k5','u5','t',?,'2024-01-01')`, sha256HexForTest(memberKey))
	req := authReq(http.MethodPatch, "/v1/users/u4/role", `{"role":"member"}`, memberKey)
	req.SetPathValue("id", "u4")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.SetUserRole)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("member changing a role: got %d; want 403", rec.Code)
	}
}

func TestSetUserRole_cannotTargetOwnerOrSelf(t *testing.T) {
	h, database, ownerKey, ownerID := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin,is_owner) VALUES ('u2','o2@example.com','Other','UTC',1,0)`)

	// Self.
	req := authReq(http.MethodPatch, "/v1/users/"+ownerID+"/role", `{"role":"member"}`, ownerKey)
	req.SetPathValue("id", ownerID)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.SetUserRole)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("self role change: got %d; want 400", rec.Code)
	}
}

func TestSetUserRole_invalidRole(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','m@example.com','Member','UTC',0)`)

	req := authReq(http.MethodPatch, "/v1/users/u2/role", `{"role":"superuser"}`, ownerKey)
	req.SetPathValue("id", "u2")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.SetUserRole)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d; want 400 (invalid role)", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// POST /v1/users/{id}/transfer-ownership  (owner only)
// ---------------------------------------------------------------------------

func TestTransferOwnership_swapsOwner(t *testing.T) {
	h, database, ownerKey, ownerID := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','next@example.com','Next','UTC',0)`)

	req := authReq(http.MethodPost, "/v1/users/u2/transfer-ownership", "", ownerKey)
	req.SetPathValue("id", "u2")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.TransferOwnership)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d — %s", rec.Code, rec.Body.String())
	}

	var oldOwnerIsOwner, oldOwnerIsAdmin, newOwnerIsOwner, newOwnerIsAdmin int
	database.QueryRow(`SELECT is_owner, is_admin FROM users WHERE id=?`, ownerID).Scan(&oldOwnerIsOwner, &oldOwnerIsAdmin)
	database.QueryRow(`SELECT is_owner, is_admin FROM users WHERE id='u2'`).Scan(&newOwnerIsOwner, &newOwnerIsAdmin)

	if oldOwnerIsOwner != 0 || oldOwnerIsAdmin != 1 {
		t.Errorf("old owner should be demoted to admin: is_owner=%d is_admin=%d", oldOwnerIsOwner, oldOwnerIsAdmin)
	}
	if newOwnerIsOwner != 1 || newOwnerIsAdmin != 1 {
		t.Errorf("new owner should be owner+admin: is_owner=%d is_admin=%d", newOwnerIsOwner, newOwnerIsAdmin)
	}

	// Exactly one owner remains.
	var owners int
	database.QueryRow(`SELECT COUNT(*) FROM users WHERE is_owner=1`).Scan(&owners)
	if owners != 1 {
		t.Errorf("owner count = %d; want exactly 1", owners)
	}
}

func TestTransferOwnership_requiresOwner(t *testing.T) {
	h, database, _, _ := setupWorkspaceWithDB(t)
	adminKey := "admin-xfer-key"
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin,is_owner) VALUES ('u2','a@example.com','Admin','UTC',1,0)`)
	database.Exec(`INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES ('k2','u2','t',?,'2024-01-01')`, sha256HexForTest(adminKey))
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','m@example.com','Member','UTC',0)`)

	req := authReq(http.MethodPost, "/v1/users/u3/transfer-ownership", "", adminKey)
	req.SetPathValue("id", "u3")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.TransferOwnership)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d; want 403", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// DELETE /v1/users/{id}  — role + booking guards
// ---------------------------------------------------------------------------

func TestDeleteUser_cannotRemoveOwner(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	// Make a second owner-less admin actor isn't needed; owner deletes... the owner.
	// Insert another user flagged as owner to attempt deletion of an owner.
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin,is_owner) VALUES ('u2','o2@example.com','Owner2','UTC',1,1)`)

	req := authReq(http.MethodDelete, "/v1/users/u2", "", ownerKey)
	req.SetPathValue("id", "u2")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.DeleteUser)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d; want 400 (cannot remove owner)", rec.Code)
	}
}

// Any admin may remove another admin (the owner still cannot be removed).
func TestDeleteUser_adminCanRemoveAdmin(t *testing.T) {
	h, database, _, _ := setupWorkspaceWithDB(t)
	adminKey := "admin-del-admin-key"
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin,is_owner) VALUES ('u2','a@example.com','Admin','UTC',1,0)`)
	database.Exec(`INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES ('k2','u2','t',?,'2024-01-01')`, sha256HexForTest(adminKey))
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','a3@example.com','Admin3','UTC',1)`)

	req := authReq(http.MethodDelete, "/v1/users/u3", "", adminKey)
	req.SetPathValue("id", "u3")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.DeleteUser)(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("got %d; want 200 (an admin may remove another admin) — %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// POST /v1/users/{id}/password — role-gated (no privilege escalation)
// ---------------------------------------------------------------------------

func TestAdminSetPassword_adminCannotResetOwner(t *testing.T) {
	h, database, _, ownerID := setupWorkspaceWithDB(t)
	adminKey := "admin-resets-owner-key"
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin,is_owner) VALUES ('u2','a@example.com','Admin','UTC',1,0)`)
	database.Exec(`INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES ('k2','u2','t',?,'2024-01-01')`, sha256HexForTest(adminKey))

	req := authReq(http.MethodPost, "/v1/users/"+ownerID+"/password", `{"password":"newpassword123"}`, adminKey)
	req.SetPathValue("id", ownerID)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.AdminSetPassword)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d; want 403 (admin cannot reset owner password) — %s", rec.Code, rec.Body.String())
	}
}

func TestAdminSetPassword_adminCannotResetAdmin(t *testing.T) {
	h, database, _, _ := setupWorkspaceWithDB(t)
	adminKey := "admin-resets-admin-key"
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin,is_owner) VALUES ('u2','a@example.com','Admin','UTC',1,0)`)
	database.Exec(`INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES ('k2','u2','t',?,'2024-01-01')`, sha256HexForTest(adminKey))
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','a3@example.com','Admin3','UTC',1)`)

	req := authReq(http.MethodPost, "/v1/users/u3/password", `{"password":"newpassword123"}`, adminKey)
	req.SetPathValue("id", "u3")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.AdminSetPassword)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d; want 403 (admin cannot reset another admin) — %s", rec.Code, rec.Body.String())
	}
}

func TestAdminSetPassword_ownerCanResetAdmin_adminCanResetMember(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	adminKey := "admin-resets-member-key"
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin,is_owner) VALUES ('u2','a@example.com','Admin','UTC',1,0)`)
	database.Exec(`INSERT INTO api_keys (id,user_id,name,key_hash,created_at) VALUES ('k2','u2','t',?,'2024-01-01')`, sha256HexForTest(adminKey))
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','m@example.com','Member','UTC',0)`)

	// Owner resets the admin's password → allowed.
	req := authReq(http.MethodPost, "/v1/users/u2/password", `{"password":"newpassword123"}`, ownerKey)
	req.SetPathValue("id", "u2")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.AdminSetPassword)(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("owner reset admin: got %d; want 200 — %s", rec.Code, rec.Body.String())
	}

	// Admin resets a plain member's password → allowed.
	req = authReq(http.MethodPost, "/v1/users/u3/password", `{"password":"newpassword123"}`, adminKey)
	req.SetPathValue("id", "u3")
	rec = httptest.NewRecorder()
	h.RequireAuth(h.AdminSetPassword)(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("admin reset member: got %d; want 200 — %s", rec.Code, rec.Body.String())
	}
}

// TestAdminSetPassword_revokesTargetSessions proves an admin force-resetting a
// compromised user's password actually evicts them — without this, a session cookie
// stolen before the reset kept working until its own TTL expired.
func TestAdminSetPassword_revokesTargetSessions(t *testing.T) {
	h, database, ownerKey, _ := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','m@example.com','Member','UTC',0)`)
	database.Exec(`INSERT INTO sessions (id,user_id,expires_at) VALUES ('victim-sess','u2','2099-01-01T00:00:00Z')`)

	req := authReq(http.MethodPost, "/v1/users/u2/password", `{"password":"newpassword123"}`, ownerKey)
	req.SetPathValue("id", "u2")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.AdminSetPassword)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d; want 200 — %s", rec.Code, rec.Body.String())
	}

	var n int
	database.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = 'victim-sess'`).Scan(&n)
	if n != 0 {
		t.Error("target's session survived an admin password reset; want it revoked")
	}
}

func TestDeleteUser_blockedByUpcomingBookings(t *testing.T) {
	h, database, ownerKey, ownerID := setupWorkspaceWithDB(t)
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','host2@example.com','Host','UTC',0)`)
	database.Exec(`INSERT INTO event_types (id,user_id,slug,name,duration_minutes) VALUES ('et1','u2','et-slug','E',30)`)
	// Future, non-cancelled booking hosted by u2.
	database.Exec(`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		VALUES ('b1','et1','u2','2099-01-01T10:00:00Z','2099-01-01T10:30:00Z','confirmed')`)

	req := authReq(http.MethodDelete, "/v1/users/u2", "", ownerKey)
	req.SetPathValue("id", "u2")
	rec := httptest.NewRecorder()
	h.RequireAuth(h.DeleteUser)(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d; want 409 (upcoming bookings block removal) — %s", rec.Code, rec.Body.String())
	}

	// Once nothing is upcoming, past and cancelled history no longer blocks: in "delete"
	// mode the member's event types and their past bookings go with them.
	database.Exec(`UPDATE bookings SET status='cancelled' WHERE id='b1'`)
	req = authReq(http.MethodDelete, "/v1/users/u2", "", ownerKey)
	req.SetPathValue("id", "u2")
	rec = httptest.NewRecorder()
	h.RequireAuth(h.DeleteUser)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("history only: got %d; want 200 — %s", rec.Code, rec.Body.String())
	}
	var left int
	database.QueryRow(`SELECT (SELECT COUNT(*) FROM bookings WHERE id='b1') + (SELECT COUNT(*) FROM event_types WHERE id='et1')`).Scan(&left)
	if left != 0 {
		t.Errorf("delete mode left %d rows of history behind; want 0", left)
	}
	database.Exec(`INSERT INTO event_types (id,user_id,slug,name,duration_minutes) VALUES ('et0',?,'et0-slug','E0',30)`, ownerID)
	database.Exec(`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		VALUES ('b1','et0',?,'2020-01-01T10:00:00Z','2020-01-01T10:30:00Z','confirmed')`, ownerID)

	// A member who only ever attended as a non-primary seat (plus a stale MCP
	// token) deletes cleanly: seats and tokens cascade away (migration 00063).
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u3','seat@example.com','Seat','UTC',0)`)
	database.Exec(`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary) VALUES ('seat1','b1','u3',0)`)
	database.Exec(`INSERT INTO oauth_access_tokens (id,token_hash,client_id,user_id,expires_at,created_at) VALUES ('tok1','hash1','c1','u3','2099-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	req = authReq(http.MethodDelete, "/v1/users/u3", "", ownerKey)
	req.SetPathValue("id", "u3")
	rec = httptest.NewRecorder()
	h.RequireAuth(h.DeleteUser)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("seat-only member delete: got %d; want 200 — %s", rec.Code, rec.Body.String())
	}
	var seats, toks int
	database.QueryRow(`SELECT COUNT(*) FROM booking_hosts WHERE user_id='u3'`).Scan(&seats)
	database.QueryRow(`SELECT COUNT(*) FROM oauth_access_tokens WHERE user_id='u3'`).Scan(&toks)
	if seats != 0 || toks != 0 {
		t.Fatalf("cascade left seats=%d tokens=%d; want 0 0", seats, toks)
	}

	// A non-primary seat on an UPCOMING booking blocks removal too: deleting
	// the member would silently drop them from a meeting that still needs them.
	database.Exec(`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u4','upcoming-seat@example.com','Upcoming','UTC',0)`)
	database.Exec(`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status)
		VALUES ('b2','et0',?,'2099-02-01T10:00:00Z','2099-02-01T10:30:00Z','confirmed')`, ownerID)
	database.Exec(`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary) VALUES ('seat2','b2','u4',0)`)
	req = authReq(http.MethodDelete, "/v1/users/u4", "", ownerKey)
	req.SetPathValue("id", "u4")
	rec = httptest.NewRecorder()
	h.RequireAuth(h.DeleteUser)(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("upcoming non-primary seat should block removal, got %d — %s", rec.Code, rec.Body.String())
	}
}

// Transfer mode: event types, past bookings and live events move to the chosen member
// and the links keep working; delete mode refuses while other hosts still have upcoming
// bookings on the member's event types.
func TestDeleteUser_transferModeKeepsEventTypesAndHistory(t *testing.T) {
	h, database, ownerKey, ownerID := setupWorkspaceWithDB(t)
	for _, q := range []string{
		`INSERT INTO users (id,email,name,iana_timezone,is_admin) VALUES ('u2','leaver@example.com','Leaver','UTC',1)`,
		`INSERT INTO event_types (id,user_id,slug,name,duration_minutes) VALUES ('etx','u2','leaver-call','Call',30)`,
		`INSERT INTO event_type_hosts (id,event_type_id,user_id,role,priority) VALUES ('h1','etx','u2','required',0)`,
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('past','etx','u2','2020-01-01T10:00:00Z','2020-01-01T10:30:00Z','confirmed')`,
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('future','etx','` + ownerID + `','2099-01-01T10:00:00Z','2099-01-01T10:30:00Z','confirmed')`,
		// An upcoming meeting the leaver hosts: transfer carries it, delete refuses.
		`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at,status) VALUES ('mine','etx','u2','2099-02-01T10:00:00Z','2099-02-01T10:30:00Z','confirmed')`,
		`INSERT INTO booking_hosts (id,booking_id,user_id,is_primary) VALUES ('bh1','mine','u2',1)`,
	} {
		if _, err := database.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	del := func(body string) *httptest.ResponseRecorder {
		req := authReq(http.MethodDelete, "/v1/users/u2", body, ownerKey)
		req.SetPathValue("id", "u2")
		rec := httptest.NewRecorder()
		h.RequireAuth(h.DeleteUser)(rec, req)
		return rec
	}
	prev := httptest.NewRecorder()
	preq := authReq(http.MethodGet, "/v1/users/u2/removal-preview", "", ownerKey)
	preq.SetPathValue("id", "u2")
	h.RequireAuth(h.GetRemovalPreview)(prev, preq)
	if !strings.Contains(prev.Body.String(), `"can_delete":false`) || !strings.Contains(prev.Body.String(), `"can_transfer":true`) {
		t.Errorf("preview = %s; want transfer-only (upcoming booking on their event type)", prev.Body.String())
	}
	if rec := del(`{"mode":"delete"}`); rec.Code != http.StatusConflict {
		t.Errorf("delete with another host's upcoming booking on their event type: %d; want 409", rec.Code)
	}
	if rec := del(`{"mode":"transfer","transfer_to":"u2"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("transfer to self: %d; want 400", rec.Code)
	}
	if rec := del(`{"mode":"transfer","transfer_to":"` + ownerID + `"}`); rec.Code != http.StatusOK {
		t.Fatalf("transfer: %d — %s", rec.Code, rec.Body.String())
	}
	var etOwner, pastHost, hostSeat string
	database.QueryRow(`SELECT user_id FROM event_types WHERE id='etx'`).Scan(&etOwner)
	database.QueryRow(`SELECT host_id FROM bookings WHERE id='past'`).Scan(&pastHost)
	database.QueryRow(`SELECT user_id FROM event_type_hosts WHERE event_type_id='etx'`).Scan(&hostSeat)
	var upcomingHost, upcomingSeat string
	database.QueryRow(`SELECT host_id FROM bookings WHERE id='mine'`).Scan(&upcomingHost)
	database.QueryRow(`SELECT user_id FROM booking_hosts WHERE booking_id='mine'`).Scan(&upcomingSeat)
	if upcomingHost != ownerID || upcomingSeat != ownerID {
		t.Errorf("upcoming meeting after transfer: host %q seat %q; want %q", upcomingHost, upcomingSeat, ownerID)
	}
	if etOwner != ownerID || pastHost != ownerID || hostSeat != ownerID {
		t.Errorf("after transfer: et owner %q, past host %q, host seat %q; want all %q", etOwner, pastHost, hostSeat, ownerID)
	}
}
