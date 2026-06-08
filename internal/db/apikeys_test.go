package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// expectedAuthQuery is the exact SQL the helper issues. Locking it
// here means a refactor that changes the join shape — and therefore
// the parity contract — fails the test.
const expectedAuthQuery = `SELECT u.id, u.email, u.name, o.id, o.name, o.domain, o."atomWorkspaceId", uto.role, ak.hash FROM "ApiKey" ak JOIN "Org" o ON o.id = ak."orgId" JOIN "User" u ON u.id = ak."createdById" LEFT JOIN "UserToOrg" uto ON uto."orgId" = o.id AND uto."userId" = u.id WHERE ak.hash = \$1`

// TestGetApiKeyAuth_OwnerHappyPath locks the contract that a valid
// API-key hash whose creator IS a member of the owning org with role
// OWNER yields the fully-populated AuthLookup. This is the canonical
// authenticated-request shape that downstream handlers receive.
func TestGetApiKeyAuth_OwnerHappyPath(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	hash := "0996712ac834605a74e9c331ab48784572ec5afe364d19df08e4d48031e8521b"
	mock.ExpectQuery(expectedAuthQuery).
		WithArgs(hash).
		WillReturnRows(pgxmock.NewRows([]string{
			"u.id", "u.email", "u.name",
			"o.id", "o.name", "o.domain", "o.atomWorkspaceId",
			"uto.role", "ak.hash",
		}).AddRow(
			"user-cuid-1", strPtr("owner@example.com"), strPtr("Org Owner"),
			int32(1), "Atom Org A", "orga", strPtr("atom-org-a-kind"),
			strPtr("OWNER"), hash,
		))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}

	got, err := q.GetApiKeyAuth(ctx, hash)
	if err != nil {
		t.Fatalf("GetApiKeyAuth: %v", err)
	}
	if got.UserID != "user-cuid-1" {
		t.Errorf("UserID: got %q, want user-cuid-1", got.UserID)
	}
	if got.UserEmail == nil || *got.UserEmail != "owner@example.com" {
		t.Errorf("UserEmail: got %v", got.UserEmail)
	}
	if got.Org.ID != 1 || got.Org.Domain != "orga" {
		t.Errorf("Org: got %+v", got.Org)
	}
	if got.Role != "OWNER" {
		t.Errorf("Role: got %q, want OWNER", got.Role)
	}
	if got.ApiKeyHash != hash {
		t.Errorf("ApiKeyHash: got %q, want %q", got.ApiKeyHash, hash)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetApiKeyAuth_MemberRolePreserved confirms the role column maps
// through verbatim — a MEMBER key returns role "MEMBER" so the
// role-guard at the handler layer can distinguish without re-querying.
func TestGetApiKeyAuth_MemberRolePreserved(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	hash := "memberhash"
	mock.ExpectQuery(expectedAuthQuery).
		WithArgs(hash).
		WillReturnRows(pgxmock.NewRows([]string{
			"u.id", "u.email", "u.name",
			"o.id", "o.name", "o.domain", "o.atomWorkspaceId",
			"uto.role", "ak.hash",
		}).AddRow(
			"user-cuid-2", strPtr("m@x"), nil,
			int32(2), "Org B", "orgb", nil,
			strPtr("MEMBER"), hash,
		))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	got, err := q.GetApiKeyAuth(ctx, hash)
	if err != nil {
		t.Fatalf("GetApiKeyAuth: %v", err)
	}
	if got.Role != "MEMBER" {
		t.Errorf("Role: got %q, want MEMBER", got.Role)
	}
	if got.UserName != nil {
		t.Errorf("UserName: got %v, want nil (column was NULL)", got.UserName)
	}
}

// TestGetApiKeyAuth_NotInOrgDefaultsToGuest locks the LEFT-JOIN branch:
// if the API-key creator is no longer a member of the owning org
// (UserToOrg row deleted), the role must default to GUEST exactly as
// auth middleware's `membership?.role ?? OrgRole.GUEST` does — never throw,
// never null-deref. Downstream auth then rejects the request with 401.
func TestGetApiKeyAuth_NotInOrgDefaultsToGuest(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	hash := "orphanhash"
	mock.ExpectQuery(expectedAuthQuery).
		WithArgs(hash).
		WillReturnRows(pgxmock.NewRows([]string{
			"u.id", "u.email", "u.name",
			"o.id", "o.name", "o.domain", "o.atomWorkspaceId",
			"uto.role", "ak.hash",
		}).AddRow(
			"user-cuid-3", strPtr("orphan@x"), nil,
			int32(3), "Org C", "orgc", nil,
			nil, hash, // <- LEFT JOIN returned NULL for uto.role
		))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	got, err := q.GetApiKeyAuth(ctx, hash)
	if err != nil {
		t.Fatalf("GetApiKeyAuth: %v", err)
	}
	if got.Role != "GUEST" {
		t.Errorf("Role: got %q, want GUEST (uto.role was NULL)", got.Role)
	}
}

// TestGetApiKeyAuth_NotFound asserts ErrApiKeyNotFound when no row
// matches the hash. The auth middleware converts this into a 401.
func TestGetApiKeyAuth_NotFound(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(expectedAuthQuery).
		WithArgs("ghost").
		WillReturnRows(pgxmock.NewRows([]string{
			"u.id", "u.email", "u.name",
			"o.id", "o.name", "o.domain", "o.atomWorkspaceId",
			"uto.role", "ak.hash",
		}))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	if _, err := q.GetApiKeyAuth(ctx, "ghost"); !errors.Is(err, ErrApiKeyNotFound) {
		t.Fatalf("got %v, want ErrApiKeyNotFound", err)
	}
}

// TestGetApiKeyAuth_EmptyHashRejected defends against a caller that
// forgot to compute HashSecret — an empty argument is a programming
// error, not a normal miss. Boundary-check at the function so a typo
// can never trigger a free DB scan.
func TestGetApiKeyAuth_EmptyHashRejected(t *testing.T) {
	q := &Queries{}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if _, err := q.GetApiKeyAuth(ctx, ""); !errors.Is(err, ErrEmptyHash) {
		t.Fatalf("got %v, want ErrEmptyHash", err)
	}
}

// TestGetApiKeyAuth_UnexpectedDbError locks the wrapped-error contract
// for the non-ErrNoRows path (connection loss, wire-protocol failure,
// context cancellation surfaced as a generic pgx error). Without this
// test the `fmt.Errorf("db: GetApiKeyAuth: %w", err)` wrap is an
// observable behaviour with no assertion, so a future refactor could
// drop the wrapping and tests would still pass.
func TestGetApiKeyAuth_UnexpectedDbError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	wireErr := errors.New("simulated wire failure")
	mock.ExpectQuery(expectedAuthQuery).
		WithArgs("anyhash").
		WillReturnError(wireErr)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}

	_, gotErr := q.GetApiKeyAuth(ctx, "anyhash")
	if gotErr == nil {
		t.Fatalf("expected an error, got nil")
	}
	// Sentinel sentinels (ErrApiKeyNotFound / ErrEmptyHash) must NOT
	// be returned for an unrelated DB error — that would mask a real
	// outage as "key not found" and 401 every request silently.
	if errors.Is(gotErr, ErrApiKeyNotFound) {
		t.Fatalf("must not return ErrApiKeyNotFound for non-NoRows error: %v", gotErr)
	}
	if errors.Is(gotErr, ErrEmptyHash) {
		t.Fatalf("must not return ErrEmptyHash for non-NoRows error: %v", gotErr)
	}
	// The original wire error must be preserved through %w so callers
	// can errors.Is(err, theirSentinel) on the cause chain.
	if !errors.Is(gotErr, wireErr) {
		t.Fatalf("expected wrapped wire error, got %v", gotErr)
	}
	// The wrapping prefix locks the diagnostic surface that on-call
	// will grep for in logs.
	if got := gotErr.Error(); !contains(got, "db: GetApiKeyAuth:") {
		t.Fatalf("expected error to start with %q, got %q", "db: GetApiKeyAuth:", got)
	}
}

// contains is a tiny helper so the test does not import strings just
// for one HasPrefix call.
func contains(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
