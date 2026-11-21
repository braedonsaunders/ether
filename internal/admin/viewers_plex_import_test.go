package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/plexwatch"
	"github.com/braedonsaunders/ether/internal/store"
)

type mockPlexServers struct {
	mu       sync.Mutex
	servers  []plexwatch.PMSServer
	accounts map[int64][]plexwatch.PMSAccount
	mapped   map[int64]plexwatch.PMSMappingInput
}

func (m *mockPlexServers) List(ctx context.Context) ([]plexwatch.PMSServer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.servers, nil
}
func (m *mockPlexServers) Add(ctx context.Context, name, baseURL, token string) (plexwatch.PMSServer, error) {
	return plexwatch.PMSServer{}, nil
}
func (m *mockPlexServers) Update(ctx context.Context, id int64, name, baseURL, token string) (plexwatch.PMSServer, error) {
	return plexwatch.PMSServer{}, nil
}
func (m *mockPlexServers) Remove(ctx context.Context, id int64) error {
	return nil
}
func (m *mockPlexServers) Accounts(ctx context.Context, id int64) ([]plexwatch.PMSAccount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.accounts[id], nil
}
func (m *mockPlexServers) MapAccount(ctx context.Context, id, accountID int64, in plexwatch.PMSMappingInput) (plexwatch.PMSServer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mapped == nil {
		m.mapped = make(map[int64]plexwatch.PMSMappingInput)
	}
	m.mapped[accountID] = in
	for i, acct := range m.accounts[id] {
		if acct.ID == accountID {
			m.accounts[id][i].UserID = in.UserID
		}
	}
	return plexwatch.PMSServer{ID: id}, nil
}
func (m *mockPlexServers) SyncNow(ctx context.Context, id int64) (plexwatch.PMSServer, error) {
	return plexwatch.PMSServer{}, nil
}
func (m *mockPlexServers) SetStepAside(ctx context.Context, id int64, stepAside bool) (plexwatch.PMSServer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.servers {
		if m.servers[i].ID == id {
			m.servers[i].StepAside = stepAside
			return m.servers[i], nil
		}
	}
	return plexwatch.PMSServer{ID: id, StepAside: stepAside}, nil
}
func (m *mockPlexServers) Unmatched(ctx context.Context, id, userID int64, after string) ([]plexwatch.PMSUnmatched, error) {
	return nil, nil
}
func (m *mockPlexServers) ExplainStepAside(ctx context.Context, key core.RatingKey) (plexwatch.StepAsideExplanation, error) {
	return plexwatch.StepAsideExplanation{Key: int64(key)}, nil
}

func TestViewersPlexImportCandidatesAndFlow(t *testing.T) {
	db := store.OpenTest(t)
	storeCfg := newStore(t, seededConfig)
	invites, err := identity.NewInvite(db, time.Hour, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}

	pms := &mockPlexServers{
		servers: []plexwatch.PMSServer{
			{ID: 10, Name: "Living Room Plex"},
		},
		accounts: map[int64][]plexwatch.PMSAccount{
			10: {
				{ID: 1, Name: "Admin", Owner: true, UserID: 1},
				{ID: 2, Name: "Kids", Owner: false, UserID: 0},
				{ID: 3, Name: "Guest", Owner: false, UserID: 0},
			},
		},
	}

	var (
		refreshed    bool
		sharedPlexMu sync.Mutex
		sharedUsers  []string
	)
	deps := admin.Deps{
		Config:       storeCfg,
		IsOwnerToken: storedOwner(storeCfg),
		PlexServers:  pms,
		Invites:      invites,
		RefreshViewers: func(ctx context.Context) error {
			refreshed = true
			return nil
		},
		SharePlexViewer: func(ctx context.Context, usernameOrEmail string, userID int64) error {
			sharedPlexMu.Lock()
			defer sharedPlexMu.Unlock()
			sharedUsers = append(sharedUsers, usernameOrEmail)
			return nil
		},
	}
	h := admin.NewHandler(deps)

	// 1. GET /ether/api/viewers/plex-candidates (verify no GET side effect / no MapAccount call)
	r := req(http.MethodGet, "/ether/api/viewers/plex-candidates", "")
	r.Header.Set("X-Plex-Token", ownerToken)
	res := serve(h, r)
	if res.Code != http.StatusOK {
		t.Fatalf("candidates status %d: %s", res.Code, res.Body)
	}

	var candidates []admin.PlexCandidateServer
	if err := json.Unmarshal(res.Body.Bytes(), &candidates); err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || len(candidates[0].Accounts) != 3 {
		t.Fatalf("expected 1 server with 3 accounts, got %+v", candidates)
	}
	if candidates[0].Accounts[1].AccountName != "Kids" || candidates[0].Accounts[1].MatchedViewerID != 0 {
		t.Fatalf("unexpected account 1: %+v", candidates[0].Accounts[1])
	}
	if len(pms.mapped) != 0 {
		t.Fatalf("GET /api/viewers/plex-candidates must not mutate mapping; mapped=%+v", pms.mapped)
	}

	// 2. POST /ether/api/viewers/import-plex (import single account)
	r = req(http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":2}`)
	r.Header.Set("X-Plex-Token", ownerToken)
	res = serve(h, r)
	if res.Code != http.StatusCreated {
		t.Fatalf("import-plex status %d: %s", res.Code, res.Body)
	}

	var created map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created["name"] != "Kids" || created["id"].(float64) <= 0 || created["inviteUrl"] == "" {
		t.Fatalf("unexpected created viewer: %+v", created)
	}
	if !refreshed {
		t.Fatal("expected viewers to be refreshed")
	}
	if pms.mapped[2].UserID != int64(created["id"].(float64)) {
		t.Fatalf("expected mapped user ID %v, got %v", created["id"], pms.mapped[2].UserID)
	}
	if pms.mapped[2].Direction != 2 {
		t.Fatalf("expected direction 2 for non-owner, got %d", pms.mapped[2].Direction)
	}

	sharedPlexMu.Lock()
	if len(sharedUsers) != 1 || sharedUsers[0] != "Kids" {
		t.Fatalf("expected SharePlexViewer called for Kids, got %v", sharedUsers)
	}
	sharedPlexMu.Unlock()

	// 3. POST /ether/api/viewers/match-plex (match to existing viewer)
	r = req(http.MethodPost, "/ether/api/viewers/match-plex", `{"serverId":10,"accountId":3,"viewerId":99}`)
	r.Header.Set("X-Plex-Token", ownerToken)
	res = serve(h, r)
	if res.Code != http.StatusOK {
		t.Fatalf("match-plex status %d: %s", res.Code, res.Body)
	}
	if pms.mapped[3].UserID != 99 {
		t.Fatalf("expected mapped user ID 99, got %v", pms.mapped[3].UserID)
	}
	if pms.mapped[3].Direction != 2 {
		t.Fatalf("expected direction 2, got %d", pms.mapped[3].Direction)
	}

	// 4. POST /ether/api/viewers/import-plex/all (import all unmapped)
	pms.mu.Lock()
	pms.accounts[10] = append(pms.accounts[10], plexwatch.PMSAccount{ID: 4, Name: "Bob", Owner: false, UserID: 0})
	pms.mu.Unlock()

	r = req(http.MethodPost, "/ether/api/viewers/import-plex/all", "")
	r.Header.Set("X-Plex-Token", ownerToken)
	res = serve(h, r)
	if res.Code != http.StatusOK {
		t.Fatalf("import-plex/all status %d: %s", res.Code, res.Body)
	}

	var imported []map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}
	if len(imported) != 4 {
		t.Fatalf("expected 4 per-account results (3 skipped, 1 imported), got %+v", imported)
	}
	var bobItem map[string]any
	for _, item := range imported {
		if item["name"] == "Bob" {
			bobItem = item
			break
		}
	}
	if bobItem == nil || bobItem["status"] != "imported" {
		t.Fatalf("expected Bob imported, got %+v", bobItem)
	}
	if pms.mapped[4].UserID != int64(bobItem["id"].(float64)) {
		t.Fatalf("expected Bob mapped to %v, got %v", bobItem["id"], pms.mapped[4].UserID)
	}

	sharedPlexMu.Lock()
	foundBob := false
	for _, u := range sharedUsers {
		if u == "Bob" {
			foundBob = true
			break
		}
	}
	sharedPlexMu.Unlock()
	if !foundBob {
		t.Fatal("expected SharePlexViewer called for Bob")
	}
}

func TestViewersPlexImportForbiddenForNonOwner(t *testing.T) {
	storeCfg := newStore(t, seededConfig)
	pms := &mockPlexServers{
		servers: []plexwatch.PMSServer{{ID: 10, Name: "Plex"}},
	}
	deps := admin.Deps{
		Config:       storeCfg,
		IsOwnerToken: storedOwner(storeCfg),
		PlexServers:  pms,
	}
	h := admin.NewHandler(deps)

	endpoints := []struct {
		method string
		url    string
		body   string
	}{
		{http.MethodGet, "/ether/api/viewers/plex-candidates", ""},
		{http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":1}`},
		{http.MethodPost, "/ether/api/viewers/match-plex", `{"serverId":10,"accountId":1,"viewerId":1}`},
		{http.MethodPost, "/ether/api/viewers/import-plex/all", ""},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.url, func(t *testing.T) {
			r := req(ep.method, ep.url, ep.body)
			r.Header.Set("X-Plex-Token", "invalid-or-non-owner-token")
			res := serve(h, r)
			if res.Code != http.StatusForbidden {
				t.Fatalf("expected 403 Forbidden, got %d: %s", res.Code, res.Body)
			}
		})
	}
}

func TestViewersPlexImportOwnerDirection(t *testing.T) {
	storeCfg := newStore(t, seededConfig)
	pms := &mockPlexServers{
		servers: []plexwatch.PMSServer{{ID: 10, Name: "Plex"}},
		accounts: map[int64][]plexwatch.PMSAccount{
			10: {
				{ID: 1, Name: "Admin", Owner: true, UserID: 0},
			},
		},
	}
	deps := admin.Deps{
		Config:       storeCfg,
		IsOwnerToken: storedOwner(storeCfg),
		PlexServers:  pms,
		ViewerDirectory: func(ctx context.Context) ([]admin.ViewerRecord, error) {
			return []admin.ViewerRecord{
				{ID: 1, Title: "Owner Viewer", IsOwner: true},
			}, nil
		},
		Invites:        &identity.Invite{},
		RefreshViewers: func(ctx context.Context) error { return nil },
	}
	h := admin.NewHandler(deps)

	r := req(http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":1}`)
	r.Header.Set("X-Plex-Token", ownerToken)
	res := serve(h, r)
	if res.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", res.Code, res.Body)
	}

	if pms.mapped[1].Direction != 3 {
		t.Fatalf("expected direction 3 (both) for owner, got %d", pms.mapped[1].Direction)
	}
	if pms.mapped[1].UserID != 1 {
		t.Fatalf("expected mapped to owner viewer ID 1, got %d", pms.mapped[1].UserID)
	}
}

func TestViewersPlexImportLongNameClamping(t *testing.T) {
	db := store.OpenTest(t)
	storeCfg := newStore(t, seededConfig)
	invites, err := identity.NewInvite(db, time.Hour, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}

	veryLongName := strings.Repeat("A", 200)
	pms := &mockPlexServers{
		servers: []plexwatch.PMSServer{{ID: 10, Name: "Plex"}},
		accounts: map[int64][]plexwatch.PMSAccount{
			10: {
				{ID: 5, Name: veryLongName, Owner: false, UserID: 0},
			},
		},
	}
	deps := admin.Deps{
		Config:         storeCfg,
		IsOwnerToken:   storedOwner(storeCfg),
		PlexServers:    pms,
		Invites:        invites,
		RefreshViewers: func(ctx context.Context) error { return nil },
	}
	h := admin.NewHandler(deps)

	r := req(http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":5}`)
	r.Header.Set("X-Plex-Token", ownerToken)
	res := serve(h, r)
	if res.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created despite 200-char name, got %d: %s", res.Code, res.Body)
	}
	var created map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	name := created["name"].(string)
	if len(name) > 120 {
		t.Fatalf("expected name clamped to <= 120 chars, got %d chars: %s", len(name), name)
	}
}

func TestViewersPlexImportTwoUsersWithSameNameDoNotMerge(t *testing.T) {
	db := store.OpenTest(t)
	storeCfg := newStore(t, seededConfig)
	invites, err := identity.NewInvite(db, time.Hour, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}

	pms := &mockPlexServers{
		servers: []plexwatch.PMSServer{{ID: 10, Name: "Plex"}},
		accounts: map[int64][]plexwatch.PMSAccount{
			10: {
				{ID: 101, Name: "Mike", Owner: false, UserID: 0},
				{ID: 102, Name: "Mike", Owner: false, UserID: 0},
			},
		},
	}
	deps := admin.Deps{
		Config:         storeCfg,
		IsOwnerToken:   storedOwner(storeCfg),
		PlexServers:    pms,
		Invites:        invites,
		RefreshViewers: func(ctx context.Context) error { return nil },
	}
	h := admin.NewHandler(deps)

	// Import first "Mike"
	r1 := req(http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":101}`)
	r1.Header.Set("X-Plex-Token", ownerToken)
	res1 := serve(h, r1)
	if res1.Code != http.StatusCreated {
		t.Fatalf("import first Mike: %d %s", res1.Code, res1.Body)
	}
	var res1Data map[string]any
	_ = json.Unmarshal(res1.Body.Bytes(), &res1Data)
	id1 := int64(res1Data["id"].(float64))

	// Import second "Mike"
	r2 := req(http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":102}`)
	r2.Header.Set("X-Plex-Token", ownerToken)
	res2 := serve(h, r2)
	if res2.Code != http.StatusCreated {
		t.Fatalf("import second Mike: %d %s", res2.Code, res2.Body)
	}
	var res2Data map[string]any
	_ = json.Unmarshal(res2.Body.Bytes(), &res2Data)
	id2 := int64(res2Data["id"].(float64))

	if id1 == id2 {
		t.Fatalf("two different PMS accounts named 'Mike' must get distinct viewers; both got %d", id1)
	}
	if pms.mapped[101].UserID == pms.mapped[102].UserID {
		t.Fatalf("both PMS accounts mapped to same user ID %d", pms.mapped[101].UserID)
	}
}

func TestViewersPlexImportShareErrorReporting(t *testing.T) {
	db := store.OpenTest(t)
	storeCfg := newStore(t, seededConfig)
	invites, err := identity.NewInvite(db, time.Hour, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}

	pms := &mockPlexServers{
		servers: []plexwatch.PMSServer{{ID: 10, Name: "Plex"}},
		accounts: map[int64][]plexwatch.PMSAccount{
			10: {
				{ID: 201, Name: "Alice", Owner: false, UserID: 0},
			},
		},
	}
	deps := admin.Deps{
		Config:       storeCfg,
		IsOwnerToken: storedOwner(storeCfg),
		PlexServers:  pms,
		Invites:      invites,
		RefreshViewers: func(ctx context.Context) error {
			return nil
		},
		SharePlexViewer: func(ctx context.Context, usernameOrEmail string, userID int64) error {
			return errors.New("simulated plex.tv network timeout")
		},
	}
	h := admin.NewHandler(deps)

	// Single import reports shareError
	r := req(http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":201}`)
	r.Header.Set("X-Plex-Token", ownerToken)
	res := serve(h, r)
	if res.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", res.Code, res.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["sharedPlex"] != false {
		t.Fatalf("expected sharedPlex=false on share failure, got %v", out["sharedPlex"])
	}
	if out["shareError"] != "simulated plex.tv network timeout" {
		t.Fatalf("expected shareError reported, got %v", out["shareError"])
	}
	if out["id"].(float64) <= 0 {
		t.Fatal("expected viewer to still be created locally")
	}
}

func TestViewersPlexImportDoesNotCreateDuplicateWhenPlexFriendSynced(t *testing.T) {
	db := store.OpenTest(t)
	storeCfg := newStore(t, seededConfig)
	invites, err := identity.NewInvite(db, time.Hour, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}

	pms := &mockPlexServers{
		servers: []plexwatch.PMSServer{{ID: 10, Name: "Plex"}},
		accounts: map[int64][]plexwatch.PMSAccount{
			10: {
				{ID: 25815033, Name: "DeLoki", Owner: false, UserID: 0},
			},
		},
	}

	var (
		directoryViewers []admin.ViewerRecord
		refreshed        bool
	)

	deps := admin.Deps{
		Config:       storeCfg,
		IsOwnerToken: storedOwner(storeCfg),
		PlexServers:  pms,
		Invites:      invites,
		ViewerDirectory: func(ctx context.Context) ([]admin.ViewerRecord, error) {
			return append([]admin.ViewerRecord(nil), directoryViewers...), nil
		},
		RefreshViewers: func(ctx context.Context) error {
			refreshed = true
			return nil
		},
		SharePlexViewer: func(ctx context.Context, usernameOrEmail string, userID int64) error {
			// Simulates production: Plex.tv share succeeds and immediately syncs the friend into auth_users
			directoryViewers = append(directoryViewers, admin.ViewerRecord{
				User: core.User{
					ID:       535,
					PlexID:   25815033,
					Username: "DeLoki",
					Title:    "DeLoki",
				},
				Origin:     "friend",
				Active:     true,
				HasPlex:    true,
				PlexShared: true,
			})
			return nil
		},
	}
	h := admin.NewHandler(deps)

	// POST /ether/api/viewers/import-plex
	r := req(http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":25815033}`)
	r.Header.Set("X-Plex-Token", ownerToken)
	res := serve(h, r)
	if res.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", res.Code, res.Body)
	}

	var created map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	if created["id"].(float64) != 535 {
		t.Fatalf("expected viewer ID 535 (the synced friend), got %v", created["id"])
	}
	if created["status"] != "imported" {
		t.Fatalf("expected status 'imported', got %v", created["status"])
	}
	if pms.mapped[25815033].UserID != 535 {
		t.Fatalf("expected PMS account 25815033 mapped to user 535, got %d", pms.mapped[25815033].UserID)
	}
	if !refreshed {
		t.Fatal("expected RefreshViewers called")
	}

	// Verify that NO local invite viewer was created in auth_users/identity_invites
	var inviteCount int
	if err := db.QueryRow(`SELECT count(*) FROM auth_users WHERE title='DeLoki'`).Scan(&inviteCount); err != nil {
		t.Fatal(err)
	}
	if inviteCount != 0 {
		t.Fatalf("expected 0 local invite viewers created in DB, got %d", inviteCount)
	}
}

func TestViewersPlexImportAllDoesNotCreateDuplicateWhenPlexFriendSynced(t *testing.T) {
	db := store.OpenTest(t)
	storeCfg := newStore(t, seededConfig)
	invites, err := identity.NewInvite(db, time.Hour, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}

	pms := &mockPlexServers{
		servers: []plexwatch.PMSServer{{ID: 10, Name: "Plex"}},
		accounts: map[int64][]plexwatch.PMSAccount{
			10: {
				{ID: 25815033, Name: "DeLoki", Owner: false, UserID: 0},
			},
		},
	}

	var directoryViewers []admin.ViewerRecord

	deps := admin.Deps{
		Config:       storeCfg,
		IsOwnerToken: storedOwner(storeCfg),
		PlexServers:  pms,
		Invites:      invites,
		ViewerDirectory: func(ctx context.Context) ([]admin.ViewerRecord, error) {
			return append([]admin.ViewerRecord(nil), directoryViewers...), nil
		},
		RefreshViewers: func(ctx context.Context) error { return nil },
		SharePlexViewer: func(ctx context.Context, usernameOrEmail string, userID int64) error {
			directoryViewers = append(directoryViewers, admin.ViewerRecord{
				User: core.User{
					ID:       535,
					PlexID:   25815033,
					Username: "DeLoki",
					Title:    "DeLoki",
				},
				Origin:     "friend",
				Active:     true,
				HasPlex:    true,
				PlexShared: true,
			})
			return nil
		},
	}
	h := admin.NewHandler(deps)

	r := req(http.MethodPost, "/ether/api/viewers/import-plex/all", "")
	r.Header.Set("X-Plex-Token", ownerToken)
	res := serve(h, r)
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", res.Code, res.Body)
	}

	var items []map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0]["id"].(float64) != 535 {
		t.Fatalf("expected viewer ID 535, got %v", items[0]["id"])
	}
	if items[0]["status"] != "imported" {
		t.Fatalf("expected status 'imported', got %v", items[0]["status"])
	}

	// Verify no local invite created in DB
	var inviteCount int
	if err := db.QueryRow(`SELECT count(*) FROM auth_users WHERE title='DeLoki'`).Scan(&inviteCount); err != nil {
		t.Fatal(err)
	}
	if inviteCount != 0 {
		t.Fatalf("expected 0 local invite viewers created in DB, got %d", inviteCount)
	}
}

func TestViewersPlexMatchOnlySharesWhenViewerIsPlexUser(t *testing.T) {
	storeCfg := newStore(t, seededConfig)
	pms := &mockPlexServers{
		servers: []plexwatch.PMSServer{{ID: 10, Name: "Plex"}},
		accounts: map[int64][]plexwatch.PMSAccount{
			10: {
				{ID: 101, Name: "LocalUser", Owner: false, UserID: 0},
				{ID: 102, Name: "PlexUser", Owner: false, UserID: 0},
			},
		},
	}

	var sharedUsers []string
	deps := admin.Deps{
		Config:       storeCfg,
		IsOwnerToken: storedOwner(storeCfg),
		PlexServers:  pms,
		ViewerDirectory: func(ctx context.Context) ([]admin.ViewerRecord, error) {
			return []admin.ViewerRecord{
				{ID: 10, Title: "LocalUser", PlexID: 0},                 // Local viewer, no Plex
				{ID: 20, Title: "PlexUser", PlexID: 102, HasPlex: true}, // Plex viewer
			}, nil
		},
		RefreshViewers: func(ctx context.Context) error { return nil },
		SharePlexViewer: func(ctx context.Context, usernameOrEmail string, userID int64) error {
			sharedUsers = append(sharedUsers, usernameOrEmail)
			return nil
		},
	}
	h := admin.NewHandler(deps)

	// 1. Match local viewer (PlexID == 0): must NOT share on Plex.tv
	r1 := req(http.MethodPost, "/ether/api/viewers/match-plex", `{"serverId":10,"accountId":101,"viewerId":10}`)
	r1.Header.Set("X-Plex-Token", ownerToken)
	res1 := serve(h, r1)
	if res1.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", res1.Code, res1.Body)
	}
	if len(sharedUsers) != 0 {
		t.Fatalf("expected SharePlexViewer NOT called for local viewer, got %v", sharedUsers)
	}

	// 2. Match Plex viewer (PlexID > 0): MUST share on Plex.tv
	r2 := req(http.MethodPost, "/ether/api/viewers/match-plex", `{"serverId":10,"accountId":102,"viewerId":20}`)
	r2.Header.Set("X-Plex-Token", ownerToken)
	res2 := serve(h, r2)
	if res2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", res2.Code, res2.Body)
	}
	if len(sharedUsers) != 1 || sharedUsers[0] != "PlexUser" {
		t.Fatalf("expected SharePlexViewer called for PlexUser, got %v", sharedUsers)
	}
}

func TestViewersPlexImportMatchedOnlySharesWhenViewerIsPlexUser(t *testing.T) {
	db := store.OpenTest(t)
	storeCfg := newStore(t, seededConfig)
	invites, err := identity.NewInvite(db, time.Hour, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}

	pms := &mockPlexServers{
		servers: []plexwatch.PMSServer{{ID: 10, Name: "Plex"}},
		accounts: map[int64][]plexwatch.PMSAccount{
			10: {
				{ID: 101, Name: "LocalUser", Owner: false, UserID: 10}, // already mapped to local viewer
				{ID: 102, Name: "PlexUser", Owner: false, UserID: 20},  // already mapped to Plex viewer
			},
		},
	}

	var sharedUsers []string
	deps := admin.Deps{
		Config:       storeCfg,
		IsOwnerToken: storedOwner(storeCfg),
		PlexServers:  pms,
		Invites:      invites,
		ViewerDirectory: func(ctx context.Context) ([]admin.ViewerRecord, error) {
			return []admin.ViewerRecord{
				{ID: 10, Title: "LocalUser", PlexID: 0},                 // Local viewer, no Plex
				{ID: 20, Title: "PlexUser", PlexID: 102, HasPlex: true}, // Plex viewer
			}, nil
		},
		RefreshViewers: func(ctx context.Context) error { return nil },
		SharePlexViewer: func(ctx context.Context, usernameOrEmail string, userID int64) error {
			sharedUsers = append(sharedUsers, usernameOrEmail)
			return nil
		},
	}
	h := admin.NewHandler(deps)

	// 1. Re-import mapped local viewer (PlexID == 0): must NOT share on Plex.tv
	r1 := req(http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":101}`)
	r1.Header.Set("X-Plex-Token", ownerToken)
	res1 := serve(h, r1)
	if res1.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", res1.Code, res1.Body)
	}
	if len(sharedUsers) != 0 {
		t.Fatalf("expected SharePlexViewer NOT called for local viewer, got %v", sharedUsers)
	}

	// 2. Re-import mapped Plex viewer (PlexID > 0): MUST share on Plex.tv
	r2 := req(http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":102}`)
	r2.Header.Set("X-Plex-Token", ownerToken)
	res2 := serve(h, r2)
	if res2.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", res2.Code, res2.Body)
	}
	if len(sharedUsers) != 1 || sharedUsers[0] != "PlexUser" {
		t.Fatalf("expected SharePlexViewer called for PlexUser, got %v", sharedUsers)
	}
}

func (m *mockPlexServers) BackfillPreview(context.Context, int64, int64) (plexwatch.PMSBackfillPreview, error) {
	return plexwatch.PMSBackfillPreview{}, nil
}

func (m *mockPlexServers) BackfillIn(context.Context, int64, int64) (plexwatch.PMSServer, error) {
	return plexwatch.PMSServer{}, nil
}

func (m *mockPlexServers) BackfillOut(context.Context, int64, int64) (plexwatch.PMSServer, error) {
	return plexwatch.PMSServer{}, nil
}

func (m *mockPlexServers) ViewerServers(context.Context, int64) ([]plexwatch.PMSViewerMapping, error) {
	return nil, nil
}

func TestViewersPlexImportResolvesSingleViaPlexUsers(t *testing.T) {
	db := store.OpenTest(t)
	storeCfg := newStore(t, seededConfig)
	invites, err := identity.NewInvite(db, time.Hour, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}

	pms := &mockPlexServers{
		servers: []plexwatch.PMSServer{{ID: 10, Name: "One.Plex"}},
		accounts: map[int64][]plexwatch.PMSAccount{
			10: {
				{ID: 2, Name: "Sakib Talukder", Owner: false, UserID: 0},
				{ID: 3, Name: "DeLoki", Owner: false, UserID: 0},
			},
		},
	}

	type shareCall struct {
		target string
		userID int64
	}
	var (
		shares  []shareCall
		ensured []plextv.SharedUser
	)

	deps := admin.Deps{
		Config:       storeCfg,
		IsOwnerToken: storedOwner(storeCfg),
		PlexServers:  pms,
		Invites:      invites,
		ListPlexUsers: func(ctx context.Context) ([]plextv.SharedUser, error) {
			return []plextv.SharedUser{
				{
					PlexID:   703916194,
					UUID:     "uuid-sakib",
					Username: "sakib99",
					Title:    "Sakib Talukder",
					Email:    "sakib@example.com",
				},
				{
					PlexID:   25815033,
					UUID:     "uuid-deloki",
					Username: "deloki_user",
					Title:    "DeLoki",
					Email:    "deloki@example.com",
				},
			}, nil
		},
		EnsurePlexFriend: func(ctx context.Context, u plextv.SharedUser) (int64, error) {
			ensured = append(ensured, u)
			if u.PlexID == 703916194 {
				return 501, nil
			}
			return 502, nil
		},
		SharePlexViewer: func(ctx context.Context, usernameOrEmail string, userID int64) error {
			shares = append(shares, shareCall{target: usernameOrEmail, userID: userID})
			return nil
		},
		RefreshViewers: func(ctx context.Context) error { return nil },
	}
	h := admin.NewHandler(deps)

	// 1. Spaced display name with local PMS account ID (2) resolves to global PlexID 703916194.
	r1 := req(http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":2}`)
	r1.Header.Set("X-Plex-Token", ownerToken)
	res1 := serve(h, r1)
	if res1.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", res1.Code, res1.Body)
	}
	var out1 map[string]any
	if err := json.Unmarshal(res1.Body.Bytes(), &out1); err != nil {
		t.Fatal(err)
	}
	if out1["id"].(float64) != 501 || out1["inviteUrl"] != "" || out1["sharedPlex"] != true {
		t.Fatalf("expected viewer 501 shared without inviteUrl, got %+v", out1)
	}

	// 2. Single-word display name with local PMS account ID (3) resolves to global PlexID 25815033.
	r2 := req(http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":3}`)
	r2.Header.Set("X-Plex-Token", ownerToken)
	res2 := serve(h, r2)
	if res2.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", res2.Code, res2.Body)
	}
	var out2 map[string]any
	if err := json.Unmarshal(res2.Body.Bytes(), &out2); err != nil {
		t.Fatal(err)
	}
	if out2["id"].(float64) != 502 || out2["inviteUrl"] != "" || out2["sharedPlex"] != true {
		t.Fatalf("expected viewer 502 shared without inviteUrl, got %+v", out2)
	}

	if len(shares) != 2 ||
		shares[0].userID != 703916194 || shares[0].target != "sakib@example.com" ||
		shares[1].userID != 25815033 || shares[1].target != "deloki@example.com" {
		t.Fatalf("unexpected SharePlexViewer calls: %+v", shares)
	}
	if len(ensured) != 2 {
		t.Fatalf("expected 2 EnsurePlexFriend calls, got %+v", ensured)
	}
}

func TestViewersPlexImportAllResolvesPlexUsersAndBatchesSync(t *testing.T) {
	db := store.OpenTest(t)
	storeCfg := newStore(t, seededConfig)
	invites, err := identity.NewInvite(db, time.Hour, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}

	pms := &mockPlexServers{
		servers: []plexwatch.PMSServer{{ID: 10, Name: "One.Plex"}},
		accounts: map[int64][]plexwatch.PMSAccount{
			10: {
				{ID: 703916194, Name: "Sakib Talukder", Owner: false, UserID: 0},
				{ID: 99, Name: "Kids Room", Owner: false, UserID: 0},
			},
		},
	}

	type shareCall struct {
		target string
		userID int64
	}
	var (
		batchShares  []shareCall
		ensured      []plextv.SharedUser
		syncCalls    int
		refreshCalls int
		legacyShares int
	)

	deps := admin.Deps{
		Config:       storeCfg,
		IsOwnerToken: storedOwner(storeCfg),
		PlexServers:  pms,
		Invites:      invites,
		ListPlexUsers: func(ctx context.Context) ([]plextv.SharedUser, error) {
			return []plextv.SharedUser{
				{
					PlexID:   703916194,
					UUID:     "uuid-sakib",
					Username: "sakib99",
					Title:    "Sakib Talukder",
					Email:    "sakib@example.com",
					Thumb:    "https://plex.tv/users/sakib/avatar",
				},
			}, nil
		},
		EnsurePlexFriend: func(ctx context.Context, u plextv.SharedUser) (int64, error) {
			ensured = append(ensured, u)
			return 501, nil
		},
		SharePlexViewer: func(ctx context.Context, usernameOrEmail string, userID int64) error {
			legacyShares++
			return nil
		},
		SharePlexViewerNoSync: func(ctx context.Context, usernameOrEmail string, userID int64) error {
			batchShares = append(batchShares, shareCall{target: usernameOrEmail, userID: userID})
			return nil
		},
		SyncPlexFriends: func(ctx context.Context) error {
			syncCalls++
			return nil
		},
		RefreshViewers: func(ctx context.Context) error {
			refreshCalls++
			return nil
		},
	}
	h := admin.NewHandler(deps)

	r := req(http.MethodPost, "/ether/api/viewers/import-plex/all", "")
	r.Header.Set("X-Plex-Token", ownerToken)
	res := serve(h, r)
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", res.Code, res.Body)
	}

	var items []map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %+v", items)
	}
	if items[0]["id"].(float64) != 501 || items[0]["inviteUrl"] != nil {
		t.Fatalf("expected Sakib Talukder imported as viewer 501 without inviteUrl, got %+v", items[0])
	}
	if items[1]["inviteUrl"] == nil || items[1]["inviteUrl"] == "" {
		t.Fatalf("expected Kids Room to fall back to invite viewer, got %+v", items[1])
	}
	if legacyShares != 0 {
		t.Fatalf("expected SharePlexViewer not called during bulk import when SharePlexViewerNoSync is set, got %d", legacyShares)
	}
	if len(batchShares) != 2 || batchShares[0].userID != 703916194 || batchShares[0].target != "sakib@example.com" {
		t.Fatalf("expected first batch share with PlexID 703916194 and email, got %+v", batchShares)
	}
	if len(ensured) != 1 || ensured[0].PlexID != 703916194 || ensured[0].Email != "sakib@example.com" {
		t.Fatalf("expected EnsurePlexFriend called for Sakib, got %+v", ensured)
	}
	if syncCalls != 1 {
		t.Fatalf("expected SyncPlexFriends called once after bulk import, got %d", syncCalls)
	}
	if refreshCalls != 1 {
		t.Fatalf("expected RefreshViewers called once after bulk import, got %d", refreshCalls)
	}
}

func TestViewersPlexImportMatchesSyncedFriendByTitle(t *testing.T) {
	db := store.OpenTest(t)
	storeCfg := newStore(t, seededConfig)
	invites, err := identity.NewInvite(db, time.Hour, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}

	pms := &mockPlexServers{
		servers: []plexwatch.PMSServer{{ID: 10, Name: "Plex"}},
		accounts: map[int64][]plexwatch.PMSAccount{
			10: {
				{ID: 2, Name: "Chrisnord Denaud", Owner: false, UserID: 0},
			},
		},
	}

	var directoryViewers []admin.ViewerRecord
	deps := admin.Deps{
		Config:       storeCfg,
		IsOwnerToken: storedOwner(storeCfg),
		PlexServers:  pms,
		Invites:      invites,
		ViewerDirectory: func(ctx context.Context) ([]admin.ViewerRecord, error) {
			return append([]admin.ViewerRecord(nil), directoryViewers...), nil
		},
		RefreshViewers: func(ctx context.Context) error { return nil },
		SharePlexViewer: func(ctx context.Context, usernameOrEmail string, userID int64) error {
			directoryViewers = append(directoryViewers, admin.ViewerRecord{
				User: core.User{
					ID:       612,
					PlexID:   999001,
					Username: "ChrisnordDenaud",
					Title:    "Chrisnord Denaud",
				},
				Origin:     "friend",
				Active:     true,
				HasPlex:    true,
				PlexShared: true,
			})
			return nil
		},
	}
	h := admin.NewHandler(deps)

	r := req(http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":2}`)
	r.Header.Set("X-Plex-Token", ownerToken)
	res := serve(h, r)
	if res.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", res.Code, res.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["id"].(float64) != 612 || out["inviteUrl"] != "" {
		t.Fatalf("expected synced friend 612 matched by Title without local invite, got %+v", out)
	}
	if pms.mapped[2].UserID != 612 {
		t.Fatalf("expected PMS account 2 mapped to 612, got %d", pms.mapped[2].UserID)
	}
}

func TestViewersPlexImportMatchesFriendlyNameAndPostSharePending(t *testing.T) {
	db := store.OpenTest(t)
	storeCfg := newStore(t, seededConfig)
	invites, err := identity.NewInvite(db, time.Hour, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}

	pms := &mockPlexServers{
		servers: []plexwatch.PMSServer{{ID: 10, Name: "Plex"}},
		accounts: map[int64][]plexwatch.PMSAccount{
			10: {
				{ID: 2, Name: "Patricson Poliard", Owner: false, UserID: 0},
				{ID: 3, Name: "dsabelag", Owner: false, UserID: 0},
			},
		},
	}

	var (
		sharedDsabelag bool
		ensured        []plextv.SharedUser
		shares         []int64
	)

	deps := admin.Deps{
		Config:       storeCfg,
		IsOwnerToken: storedOwner(storeCfg),
		PlexServers:  pms,
		Invites:      invites,
		ListPlexUsers: func(ctx context.Context) ([]plextv.SharedUser, error) {
			users := []plextv.SharedUser{
				{
					PlexID:       880011,
					Username:     "ppoliard",
					Title:        "",
					FriendlyName: "Patricson Poliard",
					Email:        "pat@example.com",
				},
			}
			if sharedDsabelag {
				users = append(users, plextv.SharedUser{
					PlexID:   990022,
					Username: "dsabelag",
					Title:    "dsabelag",
					Email:    "dsabelag@example.com",
				})
			}
			return users, nil
		},
		EnsurePlexFriend: func(ctx context.Context, u plextv.SharedUser) (int64, error) {
			ensured = append(ensured, u)
			if u.PlexID == 880011 {
				return 701, nil
			}
			return 702, nil
		},
		SharePlexViewer: func(ctx context.Context, usernameOrEmail string, userID int64) error {
			shares = append(shares, userID)
			if usernameOrEmail == "dsabelag" {
				sharedDsabelag = true
			}
			return nil
		},
		RefreshViewers: func(ctx context.Context) error { return nil },
	}
	h := admin.NewHandler(deps)

	// 1. FriendlyName match resolves Patricson Poliard to PlexID 880011.
	r1 := req(http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":2}`)
	r1.Header.Set("X-Plex-Token", ownerToken)
	res1 := serve(h, r1)
	if res1.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created for Patricson Poliard, got %d: %s", res1.Code, res1.Body)
	}
	var out1 map[string]any
	if err := json.Unmarshal(res1.Body.Bytes(), &out1); err != nil {
		t.Fatal(err)
	}
	if out1["id"].(float64) != 701 || out1["inviteUrl"] != "" {
		t.Fatalf("expected viewer 701 without inviteUrl, got %+v", out1)
	}

	// 2. dsabelag not in pre-share ListPlexUsers, appears in pending share after SharePlexViewer -> re-resolved and ensured.
	r2 := req(http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":3}`)
	r2.Header.Set("X-Plex-Token", ownerToken)
	res2 := serve(h, r2)
	if res2.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created for dsabelag, got %d: %s", res2.Code, res2.Body)
	}
	var out2 map[string]any
	if err := json.Unmarshal(res2.Body.Bytes(), &out2); err != nil {
		t.Fatal(err)
	}
	if out2["id"].(float64) != 702 || out2["inviteUrl"] != "" {
		t.Fatalf("expected viewer 702 without inviteUrl, got %+v", out2)
	}
	if len(shares) != 2 || shares[0] != 880011 {
		t.Fatalf("unexpected share userIDs: %v", shares)
	}
	if len(ensured) != 2 || ensured[0].Title != "Patricson Poliard" || ensured[1].PlexID != 990022 {
		t.Fatalf("unexpected EnsurePlexFriend calls: %+v", ensured)
	}
}

func TestViewersPlexImportFallsBackToGlobalPMSAccountID(t *testing.T) {
	db := store.OpenTest(t)
	storeCfg := newStore(t, seededConfig)
	invites, err := identity.NewInvite(db, time.Hour, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}

	pms := &mockPlexServers{
		servers: []plexwatch.PMSServer{{ID: 10, Name: "One.Plex"}},
		accounts: map[int64][]plexwatch.PMSAccount{
			10: {
				{ID: 143639010, Name: "Patricson Poliard", Owner: false, UserID: 0},
				{ID: 42, Name: "Local Kids", Owner: false, UserID: 0},
			},
		},
	}

	type shareCall struct {
		target string
		userID int64
	}
	var (
		shares  []shareCall
		ensured []plextv.SharedUser
		nextID  int64 = 800
	)
	deps := admin.Deps{
		Config:       storeCfg,
		IsOwnerToken: storedOwner(storeCfg),
		PlexServers:  pms,
		Invites:      invites,
		EnsurePlexFriend: func(ctx context.Context, u plextv.SharedUser) (int64, error) {
			ensured = append(ensured, u)
			nextID++
			return nextID, nil
		},
		SharePlexViewer: func(ctx context.Context, usernameOrEmail string, userID int64) error {
			shares = append(shares, shareCall{target: usernameOrEmail, userID: userID})
			return nil
		},
		RefreshViewers: func(ctx context.Context) error { return nil },
	}
	h := admin.NewHandler(deps)

	// 1. Single import of unresolved spaced account with global PMS ID -> passes userID 143639010 and links via EnsurePlexFriend.
	r1 := req(http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":143639010}`)
	r1.Header.Set("X-Plex-Token", ownerToken)
	res1 := serve(h, r1)
	if res1.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", res1.Code, res1.Body)
	}
	var out1 map[string]any
	if err := json.Unmarshal(res1.Body.Bytes(), &out1); err != nil {
		t.Fatal(err)
	}
	if out1["id"].(float64) != 801 || out1["inviteUrl"] != "" {
		t.Fatalf("expected viewer 801 without inviteUrl, got %+v", out1)
	}

	// 2. Single import of unresolved small-ID account -> passes userID 0 and falls back to local invite viewer.
	r2 := req(http.MethodPost, "/ether/api/viewers/import-plex", `{"serverId":10,"accountId":42}`)
	r2.Header.Set("X-Plex-Token", ownerToken)
	res2 := serve(h, r2)
	if res2.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", res2.Code, res2.Body)
	}
	var out2 map[string]any
	if err := json.Unmarshal(res2.Body.Bytes(), &out2); err != nil {
		t.Fatal(err)
	}
	if out2["inviteUrl"] == nil || out2["inviteUrl"] == "" {
		t.Fatalf("expected local inviteUrl for small account ID 42, got %+v", out2)
	}
	if len(shares) != 2 || shares[0].userID != 143639010 || shares[1].userID != 0 {
		t.Fatalf("unexpected SharePlexViewer calls: %+v", shares)
	}
	if len(ensured) != 1 || ensured[0].PlexID != 143639010 || ensured[0].Title != "Patricson Poliard" {
		t.Fatalf("unexpected EnsurePlexFriend calls: %+v", ensured)
	}

	// 3. Reset mappings and verify the same fallback behavior via bulk import.
	pms.mapped = map[int64]plexwatch.PMSMappingInput{}
	for i := range pms.accounts[10] {
		pms.accounts[10][i].UserID = 0
	}
	shares = nil
	ensured = nil

	r3 := req(http.MethodPost, "/ether/api/viewers/import-plex/all", "")
	r3.Header.Set("X-Plex-Token", ownerToken)
	res3 := serve(h, r3)
	if res3.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", res3.Code, res3.Body)
	}
	var bulk []map[string]any
	if err := json.Unmarshal(res3.Body.Bytes(), &bulk); err != nil {
		t.Fatal(err)
	}
	if len(bulk) != 2 {
		t.Fatalf("expected 2 bulk items, got %+v", bulk)
	}
	if bulk[0]["id"].(float64) != 802 || bulk[0]["inviteUrl"] != nil {
		t.Fatalf("expected global ID item linked as 802 without inviteUrl, got %+v", bulk[0])
	}
	if bulk[1]["inviteUrl"] == nil || bulk[1]["inviteUrl"] == "" {
		t.Fatalf("expected small ID item to get local inviteUrl, got %+v", bulk[1])
	}
	if len(shares) != 2 || shares[0].userID != 143639010 || shares[1].userID != 0 {
		t.Fatalf("unexpected bulk SharePlexViewer calls: %+v", shares)
	}
	if len(ensured) != 1 || ensured[0].PlexID != 143639010 || ensured[0].Title != "Patricson Poliard" {
		t.Fatalf("unexpected bulk EnsurePlexFriend calls: %+v", ensured)
	}
}

func TestViewersPlexCandidatesClassifiesPlexRelations(t *testing.T) {
	storeCfg := newStore(t, func(c *config.Config) {
		seededConfig(c)
		c.Server.MachineIdentifier = "ether-mid"
	})

	pms := &mockPlexServers{
		servers: []plexwatch.PMSServer{{ID: 10, Name: "One.Plex", MachineID: "pms-mid"}},
		accounts: map[int64][]plexwatch.PMSAccount{
			10: {
				{ID: 1, Name: "OwnerAccount", Owner: true, UserID: 1},
				{ID: 1001, Name: "Kids Home", Owner: false, UserID: 0},
				{ID: 1002, Name: "Alice Shared", Owner: false, UserID: 0},
				{ID: 1003, Name: "dsabelag", Owner: false, UserID: 0},
				{ID: 1004, Name: "Community Pal", Owner: false, UserID: 0},
				{ID: 143639010, Name: "Patricson Poliard", Owner: false, UserID: 0},
			},
		},
	}

	deps := admin.Deps{
		Config:       storeCfg,
		IsOwnerToken: storedOwner(storeCfg),
		PlexServers:  pms,
		ListPlexUsers: func(ctx context.Context) ([]plextv.SharedUser, error) {
			return []plextv.SharedUser{
				{
					PlexID:        1001,
					Username:      "kidshome",
					Title:         "Kids Home",
					Home:          true,
					AcceptedShare: true,
					Servers:       []string{"ether-mid"},
				},
				{
					PlexID:        1002,
					Username:      "alice99",
					Title:         "Alice Shared",
					AcceptedShare: true,
					Friend:        true,
					Servers:       []string{"ether-mid", "pms-mid"},
				},
				{
					PlexID:         1003,
					Username:       "dsabelag",
					Title:          "dsabelag",
					PendingShare:   true,
					PendingServers: []string{"ether-mid"},
				},
				{
					PlexID:          1004,
					Username:        "compal",
					FriendlyName:    "Community Pal",
					Friend:          true,
					CommunityFriend: true,
				},
			}, nil
		},
	}
	h := admin.NewHandler(deps)

	r := req(http.MethodGet, "/ether/api/viewers/plex-candidates", "")
	r.Header.Set("X-Plex-Token", ownerToken)
	res := serve(h, r)
	if res.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", res.Code, res.Body)
	}
	if len(pms.mapped) != 0 {
		t.Fatalf("GET /api/viewers/plex-candidates must not mutate mappings: %+v", pms.mapped)
	}

	var out []admin.PlexCandidateServer
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].PlexDirectoryTotal != 4 || len(out[0].Accounts) != 6 {
		t.Fatalf("unexpected server candidate payload: %+v", out)
	}

	byID := make(map[int64]admin.PlexCandidateAccount, len(out[0].Accounts))
	for _, a := range out[0].Accounts {
		byID[a.AccountID] = a
	}

	if got := byID[1]; got.PlexRelation != "owner" || !slices.Equal(got.PlexSources, []string{"owner"}) {
		t.Fatalf("unexpected owner classification: %+v", got)
	}
	if got := byID[1001]; got.PlexRelation != "home" || got.PlexUsername != "kidshome" || !slices.Contains(got.PlexSources, "home") {
		t.Fatalf("unexpected home classification: %+v", got)
	}
	if got := byID[1002]; got.PlexRelation != "shared" || got.PlexUsername != "alice99" ||
		!slices.Contains(got.PlexSources, "ether_share") || !slices.Contains(got.PlexSources, "pms_share") || !slices.Contains(got.PlexSources, "friend") {
		t.Fatalf("unexpected shared classification: %+v", got)
	}
	if got := byID[1003]; got.PlexRelation != "pending" || !slices.Contains(got.PlexSources, "pending_invite") {
		t.Fatalf("unexpected pending classification: %+v", got)
	}
	if got := byID[1004]; got.PlexRelation != "friend" || got.PlexUsername != "compal" ||
		!slices.Contains(got.PlexSources, "community_friend") || !slices.Contains(got.PlexSources, "friend") {
		t.Fatalf("unexpected friend classification: %+v", got)
	}
	if got := byID[143639010]; got.PlexRelation != "historical" || !slices.Equal(got.PlexSources, []string{"pms_history"}) || got.PlexUsername != "" {
		t.Fatalf("unexpected historical classification: %+v", got)
	}
}
