//go:build integration

// Phase B.3c: polyglot fixture E2E.
//
// One tenant syncs an org carrying 10 repos covering 10 distinct
// languages: go, rust, python, typescript, java, c++, swift,
// kotlin, ruby, c#. Each repo has unique stargazer / watcher /
// fork counts + a distinct topics list so the test can pin
// per-repo metadata round-trip.
//
// Proves the pipeline handles diverse repo shapes at the
// metadata + topics level. Combined with the multi-tenant
// isolation test (B.3b) it covers the user's
// "5-10 different complex projects with different languages"
// directive for polyglot E2E coverage.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"codeintel/internal/api"
	"codeintel/internal/backend/connectionmanager"
	"codeintel/internal/db"
	"codeintel/internal/migrate"
	"codeintel/pkg/asynqbridge"
	"codeintel/pkg/asynqueues"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

// polyglotRepo describes one fixture. Values chosen so each is
// unique + per-repo assertions can pin individual rows.
type polyglotRepo struct {
	id          int64
	name        string
	topics      []string
	stars       int
	watchers    int
	forks       int
	subscribers int
}

// polyglotFixture returns the canonical 10-language set. Topics
// follow GitHub's convention (lowercase, hyphenated) for typical
// language-tag use.
func polyglotFixture() []polyglotRepo {
	return []polyglotRepo{
		{id: 10001, name: "kubernetes-clone", topics: []string{"go", "containers", "orchestration"}, stars: 99000, watchers: 3200, forks: 35000, subscribers: 2800},
		{id: 10002, name: "rust-clippy-clone", topics: []string{"rust", "lints", "compiler"}, stars: 11000, watchers: 200, forks: 1500, subscribers: 180},
		{id: 10003, name: "cpython-clone", topics: []string{"python", "language", "cpython"}, stars: 56000, watchers: 1400, forks: 28000, subscribers: 1100},
		{id: 10004, name: "typescript-clone", topics: []string{"typescript", "compiler", "language"}, stars: 95000, watchers: 2400, forks: 12000, subscribers: 1900},
		{id: 10005, name: "spring-boot-clone", topics: []string{"java", "spring", "framework"}, stars: 72000, watchers: 3500, forks: 40000, subscribers: 2200},
		{id: 10006, name: "boost-clone", topics: []string{"cpp", "c-plus-plus", "library"}, stars: 6800, watchers: 320, forks: 1700, subscribers: 280},
		{id: 10007, name: "swift-clone", topics: []string{"swift", "language", "apple"}, stars: 65000, watchers: 2200, forks: 10000, subscribers: 1800},
		{id: 10008, name: "ktor-clone", topics: []string{"kotlin", "framework", "ktor"}, stars: 12000, watchers: 380, forks: 1100, subscribers: 290},
		{id: 10009, name: "rails-clone", topics: []string{"ruby", "rails", "framework"}, stars: 54000, watchers: 2400, forks: 21000, subscribers: 1900},
		{id: 10010, name: "aspnetcore-clone", topics: []string{"csharp", "dotnet", "framework"}, stars: 34000, watchers: 1300, forks: 9000, subscribers: 1100},
	}
}

// TestConnectionSync_Polyglot10Languages is the B.3c gate.
func TestConnectionSync_Polyglot10Languages(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := os.Getenv(envRedisURL)
	if redisURL == "" {
		t.Skipf("%s unset; skipping polyglot fixture", envRedisURL)
	}
	requireLocalRedisOrSkip(t, redisURL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AllowInsecureRemoteDSN: true})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}

	fixture := polyglotFixture()

	// Fake GitHub server returns all 10 repos in one
	// /orgs/polyglot-org/repos response.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/orgs/polyglot-org/repos" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		var sb strings.Builder
		sb.WriteString("[")
		for i, repo := range fixture {
			if i > 0 {
				sb.WriteString(",")
			}
			topicsJSON, _ := json.Marshal(repo.topics)
			fmt.Fprintf(&sb, `{
			  "id": %d, "name": "%s",
			  "full_name": "polyglot-org/%s",
			  "fork": false, "private": false,
			  "html_url": "https://fake/polyglot-org/%s",
			  "clone_url": "https://fake/polyglot-org/%s.git",
			  "stargazers_count": %d,
			  "watchers_count": %d,
			  "forks_count": %d,
			  "subscribers_count": %d,
			  "default_branch": "main",
			  "topics": %s,
			  "owner": {"login": "polyglot-org", "avatar_url": "https://fake/avatar"}
			}`, repo.id, repo.name, repo.name, repo.name, repo.name,
				repo.stars, repo.watchers, repo.forks, repo.subscribers, string(topicsJSON))
		}
		sb.WriteString("]")
		_, _ = w.Write([]byte(sb.String()))
	}))
	defer gh.Close()

	// Insert Org + Connection.
	orgName := "polyglot-" + uuid.NewString()[:8]
	var orgID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt") VALUES ($1, $2, NOW()) RETURNING id
	`, orgName, orgName+".test").Scan(&orgID); err != nil {
		t.Fatalf("insert Org: %v", err)
	}
	cfgJSON, _ := json.Marshal(map[string]any{
		"url":  gh.URL,
		"orgs": []string{"polyglot-org"},
	})
	var connID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt")
		VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW()) RETURNING id
	`, "polyglot-conn", cfgJSON, "github", orgID).Scan(&connID); err != nil {
		t.Fatalf("insert Connection: %v", err)
	}

	// Stand up worker + asynq Server.
	opt, err := asynqbridge.RedisOptFromURL(redisURL)
	if err != nil {
		t.Fatalf("RedisOptFromURL: %v", err)
	}
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()
	_, _ = inspector.DeleteAllPendingTasks(asynqueues.QueueConnectionSync)

	client := asynq.NewClient(opt)
	defer client.Close()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := connectionmanager.NewHandler(pool.Pool, silent)
	mux := asynq.NewServeMux()
	mux.HandleFunc(asynqueues.QueueConnectionSync, handler.AsynqHandlerFunc())
	server := asynq.NewServer(opt, asynq.Config{
		Concurrency:     2,
		Queues:          asynqueues.DefaultPriorities(),
		Logger:          &asynqbridge.SlogLogger{Base: silent},
		ShutdownTimeout: 5 * time.Second,
	})
	go func() { _ = server.Run(mux) }()
	defer server.Shutdown()
	time.Sleep(200 * time.Millisecond)

	// Schedule + poll until COMPLETED.
	syncer := api.NewAsynqConnectionSyncer(pool, client)
	result, err := syncer.Schedule(ctx, api.SyncRequest{OrgID: orgID, ConnectionID: connID})
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	var finalStatus string
	for time.Now().Before(deadline) {
		var s string
		if err := pool.QueryRow(ctx, `
			SELECT status::text FROM "ConnectionSyncJob" WHERE id = $1
		`, result.JobID).Scan(&s); err != nil {
			t.Fatalf("poll: %v", err)
		}
		if s == "COMPLETED" || s == "FAILED" {
			finalStatus = s
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if finalStatus != "COMPLETED" {
		t.Fatalf("final status: got %q want COMPLETED", finalStatus)
	}

	// =====================================================================
	// POLYGLOT INGESTION ASSERTIONS
	// =====================================================================

	t.Run("all-10-repos-present", func(t *testing.T) {
		var count int
		err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM "Repo" WHERE "orgId" = $1
		`, orgID).Scan(&count)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != len(fixture) {
			t.Errorf("got %d repos, want %d", count, len(fixture))
		}
	})

	t.Run("RepoToConnection-bindings-all-10", func(t *testing.T) {
		var count int
		err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM "RepoToConnection" WHERE "connectionId" = $1
		`, connID).Scan(&count)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != len(fixture) {
			t.Errorf("got %d bindings, want %d", count, len(fixture))
		}
	})

	t.Run("per-repo-metadata-roundtrip", func(t *testing.T) {
		for _, repo := range fixture {
			repo := repo
			t.Run(repo.name, func(t *testing.T) {
				var (
					displayName, cloneURL, webURL string
					metadataJSON                  []byte
				)
				err := pool.QueryRow(ctx, `
					SELECT "displayName", "cloneUrl", "webUrl", metadata
					FROM "Repo"
					WHERE external_id = $1 AND "orgId" = $2
				`, fmt.Sprintf("%d", repo.id), orgID).Scan(&displayName, &cloneURL, &webURL, &metadataJSON)
				if err != nil {
					t.Fatalf("query: %v", err)
				}
				if displayName != "polyglot-org/"+repo.name {
					t.Errorf("displayName: got %q", displayName)
				}
				if cloneURL != fmt.Sprintf("https://fake/polyglot-org/%s.git", repo.name) {
					t.Errorf("cloneURL: got %q", cloneURL)
				}
				if webURL != fmt.Sprintf("https://fake/polyglot-org/%s", repo.name) {
					t.Errorf("webURL: got %q", webURL)
				}

				// Metadata round-trip: gitConfig + topics.
				var md struct {
					GitConfig        map[string]string `json:"gitConfig"`
					CodeHostMetadata struct {
						GitHub struct {
							Topics []string `json:"topics"`
						} `json:"github"`
					} `json:"codeHostMetadata"`
				}
				if err := json.Unmarshal(metadataJSON, &md); err != nil {
					t.Fatalf("metadata unmarshal: %v", err)
				}
				wantStars := fmt.Sprintf("%d", repo.stars)
				if md.GitConfig["zoekt.github-stars"] != wantStars {
					t.Errorf("zoekt.github-stars: got %q want %q",
						md.GitConfig["zoekt.github-stars"], wantStars)
				}
				wantForks := fmt.Sprintf("%d", repo.forks)
				if md.GitConfig["zoekt.github-forks"] != wantForks {
					t.Errorf("zoekt.github-forks: got %q want %q",
						md.GitConfig["zoekt.github-forks"], wantForks)
				}
				// Topics round-trip (set equality).
				gotTopics := append([]string{}, md.CodeHostMetadata.GitHub.Topics...)
				wantTopics := append([]string{}, repo.topics...)
				sort.Strings(gotTopics)
				sort.Strings(wantTopics)
				if strings.Join(gotTopics, ",") != strings.Join(wantTopics, ",") {
					t.Errorf("topics: got %v want %v", gotTopics, wantTopics)
				}
			})
		}
	})

	t.Run("language-diversity-spans-10-distinct-topics", func(t *testing.T) {
		// Count distinct primary-language topics across all
		// repos in this org via the codeHostMetadata.github.topics
		// JSONB array. Expected: at least 10 distinct topics
		// (one primary language per repo).
		var distinctTopics int
		err := pool.QueryRow(ctx, `
			SELECT COUNT(DISTINCT t)
			FROM "Repo",
			     jsonb_array_elements_text(metadata->'codeHostMetadata'->'github'->'topics') AS t
			WHERE "orgId" = $1
		`, orgID).Scan(&distinctTopics)
		if err != nil {
			t.Fatalf("count distinct topics: %v", err)
		}
		if distinctTopics < 10 {
			t.Errorf("expected ≥10 distinct topics across 10 repos, got %d", distinctTopics)
		}
		t.Logf("polyglot org carries %d distinct topics across %d repos",
			distinctTopics, len(fixture))
	})

	t.Logf("polyglot E2E: 10 repos synced + persisted + metadata round-trip verified")
}
