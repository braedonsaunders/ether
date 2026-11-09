package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
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
func (m *mockPlexServers) Unmatched(ctx context.Context, id, userID int64, after string) ([]plexwatch.PMSUnmatched, error) {
	return nil, nil
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
				{ID: 10, Title: "LocalUser", PlexID: 0},               // Local viewer, no Plex
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
