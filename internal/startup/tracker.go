// Package startup tells the owner what a starting server is doing. Ether
// binds its port before it opens the database, and until the server is
// ready a Gate answers on it: the console shell and a public, read-only
// status (/ether/api/startup-status) render a status page, and every other
// route, the Plex API included, answers 503 with Retry-After. Plex clients,
// GDM discovery and plex.tv publication only ever see a ready server.
package startup

import (
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/logredact"
)

// Phase is what a starting (or stopping) server is doing.
type Phase string

const (
	PhaseStarting          Phase = "starting"           // reading settings
	PhaseRestoring         Phase = "restoring"          // swapping in a restored backup
	PhaseWaitingDatabase   Phase = "waiting_database"   // PostgreSQL unreachable; retrying
	PhaseWaitingCluster    Phase = "waiting_cluster"    // older nodes must update before migrating
	PhaseBackingUp         Phase = "backing_up"         // safety copy before migrating
	PhaseMigrating         Phase = "migrating"          // applying database updates
	PhaseImporting         Phase = "importing"          // importing a restored dump
	PhaseCheckingIntegrity Phase = "checking_integrity" // checking a restored database
	PhaseStartingServices  Phase = "starting_services"
	PhaseReady             Phase = "ready"
	PhaseStopping          Phase = "stopping" // shutting down for a restart
	PhaseFailed            Phase = "failed"
)

// Failure codes: what went wrong, so the console can say what to do.
const (
	FailDatabaseUnreachable = "database_unreachable" // PostgreSQL refused or timed out
	FailDatabaseRejected    = "database_rejected"    // bad password, missing database, invalid URL
	FailDatabaseCorrupt     = "database_corrupt"     // SQLite file damaged
	FailMigration           = "migration_failed"
	FailCredentialKey       = "credential_key"
	FailRestore             = "restore_failed" // originals could not be put back
	FailConfig              = "config"
	FailUnknown             = "unknown"
)

// Database names the database a server is opening, without credentials.
type Database struct {
	// Kind is "sqlite" or "postgres".
	Kind string `json:"kind"`
	// Host is host:port (PostgreSQL only).
	Host string `json:"host,omitempty"`
}

// Failure is why a start failed.
type Failure struct {
	Code string `json:"code"`
	// Detail is the error, with credentials scrubbed.
	Detail string `json:"detail,omitempty"`
	// Recoverable is set when the start page may restore a backup (a
	// standalone server, whose database is a file it can replace).
	Recoverable bool `json:"recoverable,omitempty"`
}

// Status is the startup status endpoint's document. It carries no
// settings and no credentials.
type Status struct {
	Phase Phase `json:"phase"`
	Ready bool  `json:"ready"`
	// Message is a one-line English summary (for logs and API clients;
	// the console words its own from Phase and the fields below).
	Message string `json:"message"`
	// Step of Steps, while a phase has countable work (migrations).
	Step  int `json:"step,omitempty"`
	Steps int `json:"steps,omitempty"`
	// Started is when this start began; Since when the phase did.
	Started time.Time `json:"started"`
	Since   time.Time `json:"since"`
	Elapsed float64   `json:"elapsedSeconds"`
	// Attempt and RetryAt while waiting for the database or after a
	// failure (the server retries by itself).
	Attempt  int        `json:"attempt,omitempty"`
	RetryAt  *time.Time `json:"retryAt,omitempty"`
	Database *Database  `json:"database,omitempty"`
	Failure  *Failure   `json:"failure,omitempty"`
	// Restoring names the backup being restored.
	Restoring string `json:"restoring,omitempty"`
	Version   string `json:"version,omitempty"`
}

// Tracker holds a start's status. It is safe for concurrent use.
type Tracker struct {
	mu  sync.Mutex
	now func() time.Time
	st  Status
}

// NewTracker starts tracking a start of version.
func NewTracker(version string, now func() time.Time) *Tracker {
	if now == nil {
		now = time.Now
	}
	t := now().UTC()
	return &Tracker{now: now, st: Status{Phase: PhaseStarting, Message: messages[PhaseStarting], Started: t, Since: t, Version: version}}
}

var messages = map[Phase]string{
	PhaseStarting:          "Starting Ether",
	PhaseRestoring:         "Restoring a backup",
	PhaseWaitingDatabase:   "Waiting for the database",
	PhaseWaitingCluster:    "Waiting for other cluster nodes to update before updating the shared database",
	PhaseBackingUp:         "Saving a safety copy of the database before updating it",
	PhaseMigrating:         "Updating the database",
	PhaseImporting:         "Importing the restored data",
	PhaseCheckingIntegrity: "Checking the restored database",
	PhaseStartingServices:  "Starting services",
	PhaseReady:             "Ether is ready",
	PhaseStopping:          "Ether is stopping to restart",
	PhaseFailed:            "Ether could not start",
}

// Begin starts a new start: a restart within the process (a cluster
// transition or a retry after a failure) is timed from here.
func (t *Tracker) Begin() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now().UTC()
	db, version := t.st.Database, t.st.Version
	t.st = Status{Phase: PhaseStarting, Message: messages[PhaseStarting], Started: now, Since: now, Database: db, Version: version}
}

// Set moves to phase, clearing the previous phase's details.
func (t *Tracker) Set(phase Phase) {
	t.update(func(s *Status) {
		if s.Phase != phase {
			s.Since = t.now().UTC()
		}
		s.Phase, s.Message, s.Ready = phase, messages[phase], phase == PhaseReady
		s.Step, s.Steps, s.Attempt, s.RetryAt, s.Failure = 0, 0, 0, nil, nil
		if phase != PhaseRestoring {
			s.Restoring = ""
		}
	})
}

// Progress records step of steps within the current phase.
func (t *Tracker) Progress(step, steps int) {
	t.update(func(s *Status) { s.Step, s.Steps = step, steps })
}

// Restoring records the backup being restored.
func (t *Tracker) Restoring(source string) {
	t.Set(PhaseRestoring)
	t.update(func(s *Status) { s.Restoring = source })
}

// SetDatabase records which database is being opened.
func (t *Tracker) SetDatabase(db Database) {
	t.update(func(s *Status) { s.Database = &db })
}

// Waiting records a wait (for the database, or for other nodes) with the
// attempt that failed and when the next one is due.
func (t *Tracker) Waiting(phase Phase, attempt int, retry time.Duration, err error) {
	t.Set(phase)
	t.update(func(s *Status) {
		at := t.now().UTC().Add(retry)
		s.Attempt, s.RetryAt = attempt, &at
		if err != nil && phase == PhaseWaitingDatabase {
			s.Failure = &Failure{Code: FailDatabaseUnreachable, Detail: scrub(err)}
		}
	})
}

// Fail records a failed start the server will retry after retry.
func (t *Tracker) Fail(f Failure, attempt int, retry time.Duration) {
	t.Set(PhaseFailed)
	t.update(func(s *Status) {
		f.Detail = scrubText(f.Detail)
		s.Failure, s.Attempt = &f, attempt
		if retry > 0 {
			at := t.now().UTC().Add(retry)
			s.RetryAt = &at
		}
	})
}

// Postpone moves a failed start's next retry to retry from now (the owner
// is using the recovery page).
func (t *Tracker) Postpone(retry time.Duration) {
	t.update(func(s *Status) {
		if s.Phase == PhaseFailed {
			at := t.now().UTC().Add(retry)
			s.RetryAt = &at
		}
	})
}

// Snapshot returns the current status.
func (t *Tracker) Snapshot() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.st
	s.Elapsed = t.now().Sub(s.Started).Seconds()
	if s.Database != nil {
		db := *s.Database
		s.Database = &db
	}
	if s.Failure != nil {
		f := *s.Failure
		s.Failure = &f
	}
	return s
}

// Phase is the current phase.
func (t *Tracker) Phase() Phase {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.st.Phase
}

func (t *Tracker) update(fn func(*Status)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fn(&t.st)
}

// maxDetail bounds an error shown on the public status page.
const maxDetail = 400

func scrub(err error) string { return scrubText(err.Error()) }

var (
	// absPath matches an absolute file path (Unix, or Windows with a drive).
	absPath = regexp.MustCompile(`(?:[A-Za-z]:)?(?:[/\\][^\s/\\:"'()\[\]{},;]+){2,}`)
	// dbUser matches a PostgreSQL user name in a connection error.
	dbUser = regexp.MustCompile(`(?i)(\buser[= ]+)"?[^\s"'),;]+"?`)
)

// scrubText trims an error message for the public status page: no
// credentials (URL passwords, tokens), no database user names, file paths
// reduced to the file name, and short.
func scrubText(s string) string {
	s = logredact.String(s)
	s = absPath.ReplaceAllStringFunc(s, func(p string) string {
		if i := strings.LastIndexAny(p, `/\`); i >= 0 {
			return p[i+1:]
		}
		return p
	})
	s = dbUser.ReplaceAllString(s, "${1}…")
	s = strings.TrimSpace(s)
	if len(s) > maxDetail {
		s = s[:maxDetail] + "…"
	}
	return s
}
