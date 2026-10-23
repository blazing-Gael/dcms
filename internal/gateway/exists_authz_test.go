package gateway_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/blazing-Gael/dcms/internal/store"
)

// Membership access rules (ADR-0036, issue #64): read/write may depend on a row in
// another collection — the edtech case of enrollment-gated lessons, plus the
// `all:`/`any:` boolean composites the same change introduces.

const existsSchema = `
version: "1"
auth:
  roles:
    admin:   { label: Administrator }
    teacher: { label: Teacher }
  session:
    ttl: 1h
collections:
  courses:
    fields:
      title: { type: string }
    access:
      read: public
      create: [admin]
  enrollments:
    fields:
      user:   { type: relation, target: _users, required: true }
      course: { type: relation, target: courses, required: true }
      status: { type: enum, values: [active, cancelled] }
    access:
      read:   [admin]
      create: [admin]
  lessons:
    fields:
      course: { type: relation, target: courses, required: true }
      title:  { type: string }
    access:
      read:
        any:
          - admin
          - exists:
              in: enrollments
              match: { course: course }
              principal: user
              where: { status: active }
      create: [admin]
  assignments:
    fields:
      course: { type: relation, target: courses, required: true }
      title:  { type: string }
    access:
      read:
        all:
          - teacher
          - exists: { in: enrollments, match: { course: course }, principal: user }
      create: [admin]
`

// seedExistsFixture builds: two courses; alice (teacher) enrolled active in c1;
// dave (teacher) enrolled in nothing; carol (no role) enrolled active in c1; bob
// (no role) enrolled in nothing. Plus one lesson/assignment per course.
type existsFixture struct {
	c1, c2 string
	l1, l2 string
	a1     string
}

func seedExistsFixture(t *testing.T, db store.Adapter) existsFixture {
	t.Helper()
	mk := func(coll string, data store.Record) string {
		rec, err := db.Create(context.Background(), store.WriteInput{Collection: coll, Data: data})
		if err != nil {
			t.Fatalf("seed %s: %v", coll, err)
		}
		return rec["id"].(string)
	}
	var f existsFixture
	f.c1 = mk("courses", store.Record{"title": "Course 1"})
	f.c2 = mk("courses", store.Record{"title": "Course 2"})

	alice := seedUserID(t, db, "alice@x.com", "pw-alice-1234", "teacher")
	seedUserID(t, db, "dave@x.com", "pw-dave-12345", "teacher")
	carol := seedUserID(t, db, "carol@x.com", "pw-carol-1234")
	seedUserID(t, db, "bob@x.com", "pw-bob-123456")

	mk("enrollments", store.Record{"user": alice, "course": f.c1, "status": "active"})
	mk("enrollments", store.Record{"user": carol, "course": f.c1, "status": "active"})

	f.l1 = mk("lessons", store.Record{"course": f.c1, "title": "L1"})
	f.l2 = mk("lessons", store.Record{"course": f.c2, "title": "L2"})
	f.a1 = mk("assignments", store.Record{"course": f.c1, "title": "A1"})
	return f
}

func TestExists_EnrollmentGatedLessons(t *testing.T) {
	srv, db := newServerWith(t, existsSchema)
	seedUser(t, db, "root@x.com", "pw-root-12345", "admin")
	f := seedExistsFixture(t, db)

	admin := login(t, srv.URL, "root@x.com", "pw-root-12345")
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-1234") // enrolled active in c1
	carol := login(t, srv.URL, "carol@x.com", "pw-carol-1234") // enrolled active in c1
	bob := login(t, srv.URL, "bob@x.com", "pw-bob-123456")     // enrolled in nothing

	// List: alice/carol see only c1's lesson; bob sees none; admin sees all.
	if _, b := doAs(t, http.MethodGet, srv.URL+"/api/v1/lessons", alice, ""); listLen(t, b) != 1 {
		t.Fatalf("alice lessons: want 1 (enrolled in c1), got %v", b)
	}
	if _, b := doAs(t, http.MethodGet, srv.URL+"/api/v1/lessons", carol, ""); listLen(t, b) != 1 {
		t.Fatalf("carol lessons: want 1, got %v", b)
	}
	if _, b := doAs(t, http.MethodGet, srv.URL+"/api/v1/lessons", bob, ""); listLen(t, b) != 0 {
		t.Fatalf("bob lessons: want 0 (not enrolled), got %v", b)
	}
	if _, b := doAs(t, http.MethodGet, srv.URL+"/api/v1/lessons", admin, ""); listLen(t, b) != 2 {
		t.Fatalf("admin lessons: want 2 (bypass), got %v", b)
	}

	// Single read honours the same rule: alice reads c1's lesson (200), not c2's (404,
	// no existence leak); bob reads neither.
	if st, _ := doAs(t, http.MethodGet, srv.URL+"/api/v1/lessons/"+f.l1, alice, ""); st != http.StatusOK {
		t.Fatalf("alice read l1: got %d, want 200", st)
	}
	if st, _ := doAs(t, http.MethodGet, srv.URL+"/api/v1/lessons/"+f.l2, alice, ""); st != http.StatusNotFound {
		t.Fatalf("alice read l2 (not enrolled in c2): got %d, want 404", st)
	}
	if st, _ := doAs(t, http.MethodGet, srv.URL+"/api/v1/lessons/"+f.l1, bob, ""); st != http.StatusNotFound {
		t.Fatalf("bob read l1: got %d, want 404", st)
	}
}

// The `where: { status: active }` clause excludes a cancelled enrollment.
func TestExists_WhereExcludesCancelled(t *testing.T) {
	srv, db := newServerWith(t, existsSchema)
	f := seedExistsFixture(t, db)
	// Give carol a *cancelled* enrollment in c2 — it must not grant c2's lesson.
	if _, err := db.Create(context.Background(), store.WriteInput{Collection: "enrollments",
		Data: store.Record{"user": seedUserID(t, db, "cancel@x.com", "pw-cancel-123"), "course": f.c2, "status": "cancelled"}}); err != nil {
		t.Fatalf("seed cancelled enrollment: %v", err)
	}
	tok := login(t, srv.URL, "cancel@x.com", "pw-cancel-123")
	if _, b := doAs(t, http.MethodGet, srv.URL+"/api/v1/lessons", tok, ""); listLen(t, b) != 0 {
		t.Fatalf("cancelled enrollment must grant nothing, got %v", b)
	}
	if st, _ := doAs(t, http.MethodGet, srv.URL+"/api/v1/lessons/"+f.l2, tok, ""); st != http.StatusNotFound {
		t.Fatalf("cancelled read l2: got %d, want 404", st)
	}
}

// `all: [teacher, exists]` requires BOTH the role and the membership (ADR-0036).
func TestExists_AllRoleAndMembership(t *testing.T) {
	srv, db := newServerWith(t, existsSchema)
	seedExistsFixture(t, db)

	alice := login(t, srv.URL, "alice@x.com", "pw-alice-1234") // teacher + enrolled
	dave := login(t, srv.URL, "dave@x.com", "pw-dave-12345")   // teacher, not enrolled
	carol := login(t, srv.URL, "carol@x.com", "pw-carol-1234") // enrolled, not a teacher

	// Teacher AND enrolled → sees the assignment.
	if _, b := doAs(t, http.MethodGet, srv.URL+"/api/v1/assignments", alice, ""); listLen(t, b) != 1 {
		t.Fatalf("alice assignments: want 1 (teacher+enrolled), got %v", b)
	}
	// Teacher but enrolled in nothing → allowed to list, but the membership filter
	// leaves it empty (not a 403 — she passes the role gate).
	if st, b := doAs(t, http.MethodGet, srv.URL+"/api/v1/assignments", dave, ""); st != http.StatusOK || listLen(t, b) != 0 {
		t.Fatalf("dave assignments: want 200 + 0 rows, got %d %v", st, b)
	}
	// Not a teacher at all → the AND denies the whole rule → 403 on the list.
	if st, _ := doAs(t, http.MethodGet, srv.URL+"/api/v1/assignments", carol, ""); st != http.StatusForbidden {
		t.Fatalf("carol assignments (not a teacher): got %d, want 403", st)
	}
}

const existsSoftDeleteSchema = `
version: "1"
auth:
  roles:
    admin: { label: Administrator }
  session: { ttl: 1h }
collections:
  courses:
    fields: { title: { type: string } }
    access: { read: public, create: [admin] }
  enrollments:
    soft_delete: true
    fields:
      user:   { type: relation, target: _users }
      course: { type: relation, target: courses }
    access: { read: [admin], create: [admin], delete: [admin] }
  lessons:
    fields:
      course: { type: relation, target: courses }
    access:
      read:
        any:
          - admin
          - exists: { in: enrollments, match: { course: course }, principal: user }
      create: [admin]
`

// A soft-deleted (trashed) membership row must stop granting access — e.g. a refunded
// enrollment (ADR-0036 soft-delete handling).
func TestExists_SoftDeletedMembershipRevokes(t *testing.T) {
	srv, db := newServerWith(t, existsSoftDeleteSchema)
	seedUser(t, db, "root@x.com", "pw-root-12345", "admin")
	aliceID := seedUserID(t, db, "alice@x.com", "pw-alice-1234")
	admin := login(t, srv.URL, "root@x.com", "pw-root-12345")
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-1234")

	_, cr := doAs(t, http.MethodPost, srv.URL+"/api/v1/courses", admin, `{"title":"Go 101"}`)
	courseID := recordID(t, cr)
	doAs(t, http.MethodPost, srv.URL+"/api/v1/lessons", admin, `{"course":"`+courseID+`"}`)
	_, er := doAs(t, http.MethodPost, srv.URL+"/api/v1/enrollments", admin, `{"user":"`+aliceID+`","course":"`+courseID+`"}`)
	enrollID := recordID(t, er)

	// Enrolled → sees the lesson.
	if _, b := doAs(t, http.MethodGet, srv.URL+"/api/v1/lessons", alice, ""); listLen(t, b) != 1 {
		t.Fatalf("before trash: want 1 lesson, got %v", b)
	}
	// Admin trashes the enrollment (soft delete).
	if st, _ := doAs(t, http.MethodDelete, srv.URL+"/api/v1/enrollments/"+enrollID, admin, ""); st != http.StatusNoContent {
		t.Fatalf("trash enrollment: got %d, want 204", st)
	}
	// Access is revoked immediately — the trashed row no longer counts.
	if _, b := doAs(t, http.MethodGet, srv.URL+"/api/v1/lessons", alice, ""); listLen(t, b) != 0 {
		t.Fatalf("after trash: want 0 lessons, got %v", b)
	}
}

// Anonymous callers can never satisfy an `exists` rule (no principal to bind).
func TestExists_AnonymousDenied(t *testing.T) {
	srv, db := newServerWith(t, existsSchema)
	f := seedExistsFixture(t, db)
	if _, b := doAs(t, http.MethodGet, srv.URL+"/api/v1/lessons", "", ""); listLen(t, b) != 0 {
		t.Fatalf("anonymous lessons: want 0, got %v", b)
	}
	if st, _ := doAs(t, http.MethodGet, srv.URL+"/api/v1/lessons/"+f.l1, "", ""); st != http.StatusNotFound {
		t.Fatalf("anonymous read l1: got %d, want 404", st)
	}
}
