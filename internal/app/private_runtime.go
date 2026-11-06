package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/netutil"

	"github.com/braedonsaunders/ether/internal/cluster"
	"github.com/braedonsaunders/ether/internal/clusterconfig"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/hoststat"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/placement"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// privatePlayback separates the app lifecycle from remote operation ownership.
// Cleanup may block on backend retirement and never runs on Tick/heartbeat.
type privatePlayback interface {
	Handler() http.Handler
	Tick(context.Context) error
	Cleanup()
	Close() error
	Ready() bool
}

type deploymentRuntime struct {
	sourceAdmin   http.Handler
	publicReady   atomic.Bool
	quotaOpen     atomic.Bool
	quota         atomic.Pointer[runtimeQuotaSnapshot]
	encoder       *transcode.Manager
	placement     *placement.Service
	settings      clusterconfig.Deployment
	guard         *nodewire.Guard
	playback      privatePlayback
	telemetry     *hoststat.NodeResources
	ready         atomic.Bool
	started       atomic.Bool
	protocol      atomic.Uint32
	probes        *http.Server
	probeListener net.Listener
	probeDone     chan struct{}
	errors        chan error
	closeOnce     sync.Once
	readinessLoss atomic.Pointer[privateReadinessSnapshot]
}

func (a *App) prepareDeployment(ctx context.Context) error {
	path := os.Getenv("ETHER_DEPLOYMENT_FILE")
	if path == "" {
		return nil
	}
	d, err := clusterconfig.ReadDeployment(path)
	if err != nil {
		return err
	}
	if !d.Roles.FullServer() && !(len(d.Roles) == 1 && d.Roles[0] == config.RoleTranscode) {
		return errors.New("specialist runtime is not yet enabled for this role set")
	}
	p := &deploymentRuntime{settings: d, errors: make(chan error, 3), probeDone: make(chan struct{})}
	a.deployment = p
	mux := http.NewServeMux()
	probe := func(healthy func() bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !healthy() {
				http.Error(w, "unavailable", 503)
				return
			}
			w.WriteHeader(200)
		}
	}
	mux.HandleFunc("GET /livez", probe(func() bool { return true }))
	mux.HandleFunc("GET /startupz", probe(p.started.Load))
	mux.HandleFunc("GET /readyz", probe(p.ready.Load))
	mux.HandleFunc("GET /admissionz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !a.privateAdmissionReady() {
			w.WriteHeader(503)
		}
		_ = json.NewEncoder(w).Encode(struct {
			Accepting bool                   `json:"accepting"`
			Ledger    *placement.QuotaStatus `json:"ledger,omitempty"`
		}{a.privateAdmissionReady(), p.quotaStatus()})
	})
	p.probes = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10}
	p.probeListener, err = net.Listen("tcp", d.ProbeListen)
	if err != nil {
		return errors.New("deployment: cannot bind probe listener")
	}
	go func() {
		defer close(p.probeDone)
		if err := p.probes.Serve(p.probeListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.errors <- err
		}
	}()
	a.closers = append(a.closers, func() {
		p.closeOnce.Do(func() { p.ready.Store(false); p.protocol.Store(0); _ = p.probes.Close(); <-p.probeDone })
	})
	o, err := d.ProvisionOptions(a.opts.DataDir)
	if err != nil {
		return err
	}
	identity, err := cluster.LoadIdentity(a.opts.DataDir)
	if err != nil {
		return err
	}
	var local struct {
		Database struct {
			DeferMigrations bool `json:"deferMigrations"`
		} `json:"database"`
	}
	raw, readErr := os.ReadFile(filepath.Join(a.opts.DataDir, "config.json"))
	if readErr == nil {
		if json.Unmarshal(raw, &local) != nil {
			return errors.New("deployment: local settings are invalid")
		}
	} else if !os.IsNotExist(readErr) {
		return errors.New("deployment: cannot inspect local migration settings")
	}
	o.MigrationGate = startupMigrationsGate(identity.NodeID, local.Database.DeferMigrations, clusterLiveAfter, time.Now)
	if err := clusterconfig.Provision(ctx, o); err != nil {
		return err
	}
	return nil
}

func (a *App) buildPrivateCredentials() error {
	p := a.deployment
	if p == nil {
		return nil
	}
	n := a.cluster.Self()
	self := nodewire.Identity{Cluster: a.cfg.Get().Server.MachineIdentifier, NodeID: n.ID, Boot: n.Incarnation}
	snapshot, err := issuePrivateSnapshot(p.settings, self)
	if err != nil {
		return err
	}
	p.guard, err = nodewire.NewGuard(snapshot, func(ctx context.Context, id nodewire.Identity, _ *x509.Certificate) error {
		_, err := a.privateMember(ctx, id)
		return err
	}, time.Now)
	if err != nil {
		return err
	}
	paths := []string{}
	cfg := a.cfg.Get()
	for _, path := range []string{cfg.Transcode.TempDir, cfg.Cache.Dir} {
		if path != "" {
			seen := false
			for _, old := range paths {
				if old == path {
					seen = true
				}
			}
			if !seen {
				paths = append(paths, path)
			}
		}
	}
	p.telemetry, err = hoststat.NewNodeResources(hoststat.NodeOptions{CgroupV2Path: p.settings.CgroupV2Path, ScratchPaths: paths})
	return err
}

func (a *App) startPrivate(ctx context.Context) (func(), error) {
	p := a.deployment
	if p == nil {
		return func() {}, nil
	}
	if p.playback == nil || !p.playback.Ready() {
		a.recordPrivateReadiness(privateReadinessSnapshot{reason: readinessRuntime})
		return nil, errors.New("deployment: compatible private playback services are not ready")
	}
	ln, err := net.Listen("tcp", p.settings.PrivateListen)
	if err != nil {
		return nil, errors.New("deployment: cannot bind private listener")
	}
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := p.guard.Request(r); err != nil {
			http.Error(w, "denied", 403)
			return
		}
		if p.sourceAdmin != nil && r.URL.Path == "/cluster/runtime/source-admin" {
			p.sourceAdmin.ServeHTTP(w, r)
			return
		}
		p.playback.Handler().ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: mux, TLSConfig: p.guard.ServerTLS(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	a.closers = append(a.closers, cancel)
	var workers sync.WaitGroup
	launch := func(f func()) { workers.Add(1); go func() { defer workers.Done(); f() }() }
	launch(func() {
		err := srv.Serve(tls.NewListener(netutil.LimitListener(ln, 256), srv.TLSConfig))
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.ready.Store(false)
			p.protocol.Store(0)
			a.recordPrivateReadiness(privateReadinessSnapshot{reason: readinessListener, errorClass: privateReadinessError(err)})
			select {
			case p.errors <- err:
			default:
			}
		}
	})
	launch(func() { _ = p.telemetry.Run(runCtx) })
	launch(func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		rotation := time.NewTicker(time.Hour)
		defer rotation.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-tick.C:
				op, c := context.WithTimeout(runCtx, 3*time.Second)
				a.pollDrain(op)
				_ = a.refreshPrivateQuota(op)
				var observation privateReadinessSnapshot
				err := a.reconcilePrivatePlacement(op, &observation)
				_ = p.playback.Tick(op) // pending retirement is healthy retained ownership, not readiness loss
				c()
				p.ready.Store(err == nil && a.privateServicesReadyObserved(&observation))
				if p.ready.Load() {
					p.started.Store(true)
				}
				if p.ready.Load() {
					p.protocol.Store(1)
				} else {
					p.protocol.Store(0)
				}
				a.recordPrivateReadiness(observation)
			case <-rotation.C:
				// Configuration/identity are immutable for this process. Only revalidated
				// issuer/root contents rotate; no caller document is applied at renewal.
				o, trustErr := p.settings.ProvisionOptions(a.opts.DataDir)
				if trustErr != nil || a.cfg.KeyFingerprint() != config.KeyFingerprint(o.Key) {
					p.ready.Store(false)
					p.protocol.Store(0)
					a.recordPrivateReadiness(privateReadinessSnapshot{reason: readinessIdentity, errorClass: privateReadinessError(trustErr)})
					select {
					case p.errors <- errors.New("deployment: mounted cluster identity changed"):
					default:
					}
					return
				}
				snap, err := issuePrivateSnapshot(p.settings, nodewire.Identity{Cluster: a.cfg.Get().Server.MachineIdentifier, NodeID: a.cluster.Self().ID, Boot: a.cluster.Self().Incarnation})
				if err == nil {
					err = p.guard.Replace(snap)
				}
				if err != nil {
					p.ready.Store(false)
					p.protocol.Store(0)
					a.recordPrivateReadiness(privateReadinessSnapshot{reason: readinessRotation, errorClass: privateReadinessError(err)})
					select {
					case p.errors <- errors.New("deployment: private credential rotation failed"):
					default:
					}
					return
				}
			}
		}
	})
	// A single independent cleanup executor bounds concurrency. It can wait for
	// source Close without blocking Tick, admission expiry or cluster heartbeat.
	launch(func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-tick.C:
				p.playback.Cleanup()
			}
		}
	})
	initial, done := context.WithTimeout(runCtx, 3*time.Second)
	var observation privateReadinessSnapshot
	err = a.refreshPrivateQuota(initial)
	if err != nil {
		observation = privateReadinessSnapshot{reason: readinessQuota, at: time.Now(), errorClass: privateReadinessError(err)}
		observation.deadline, _ = initial.Deadline()
	}
	if err == nil {
		err = a.reconcilePrivatePlacement(initial, &observation)
	}
	done()
	if err != nil {
		a.recordPrivateReadiness(observation)
		cancel()
		_ = srv.Close()
		workers.Wait()
		return nil, err
	}
	if err := clusterconfig.WriteDrainControl(a.opts.DataDir, clusterconfig.DrainStatusFile, clusterconfig.DrainControl{Boot: a.cluster.Self().Incarnation, State: "running", Pending: -1}); err != nil {
		cancel()
		_ = srv.Close()
		workers.Wait()
		return nil, err
	}
	p.ready.Store(a.privateServicesReadyObserved(&observation))
	if p.ready.Load() {
		p.started.Store(true)
		p.protocol.Store(1)
	}
	a.recordPrivateReadiness(observation)
	return func() {
		a.drainPrivate()
		p.protocol.Store(0)
		_ = p.playback.Close()
		p.ready.Store(false)
		a.recordPrivateReadiness(privateReadinessSnapshot{reason: readinessClosed})
		cancel()
		_ = srv.Close()
		workers.Wait()
	}, nil
}

func (a *App) privateProtocol() uint32 {
	if a.deployment == nil {
		return 0
	}
	return a.deployment.protocol.Load()
}
func (a *App) privateErrors() <-chan error {
	if a.deployment == nil {
		return nil
	}
	return a.deployment.errors
}

func (a *App) runSpecialist(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.bindRestart(ctx, cancel)
	var background backgroundGroup
	defer background.Wait()
	defer cancel()
	if a.load != nil {
		background.start(func() { a.load.Run(ctx) })
	}
	if a.cache != nil {
		background.start(func() { a.cache.Run(ctx) })
	}
	if err := a.startCluster(ctx); err != nil {
		return err
	}
	defer a.cluster.Close()
	background.start(func() { a.runNodeControl(ctx) })
	if a.fetcher != nil {
		background.start(func() { a.runBudgets(ctx) })
		background.start(func() { a.runEgress(ctx) })
	}
	stop, err := a.startPrivate(ctx)
	if err != nil {
		return err
	}
	defer stop()
	select {
	case <-ctx.Done():
		return a.runResult()
	case err := <-a.clusterFatal:
		return err
	case err := <-a.privateErrors():
		return err
	}
}

// Public-role admission becomes ready only after the actual public listener and
// its complete handler have been installed. Private-only workers need no public
// frontend. Transient health loss does not clear the completed startup probe.
func (a *App) privateServicesReady() bool {
	return a.privateServicesReadyObserved(nil)
}

func (a *App) privateServicesReadyObserved(observation *privateReadinessSnapshot) bool {
	if observation != nil {
		observation.reason, observation.at = readinessRuntime, time.Now()
		observation.errorClass, observation.deadline = "none", time.Time{}
	}
	p := a.deployment
	if p == nil || p.playback == nil || !p.playback.Ready() {
		return false
	}
	if observation != nil {
		observation.reason = readinessPublic
	}
	ready := !(p.settings.Roles.Has(config.RoleServer) || p.settings.Roles.Has(config.RoleDelivery)) || p.publicReady.Load()
	if ready && observation != nil {
		*observation = privateReadinessSnapshot{}
	}
	return ready
}
func (a *App) markPublicReady() {
	if p := a.deployment; p != nil {
		p.publicReady.Store(true)
		if a.privateServicesReady() {
			p.started.Store(true)
			p.ready.Store(true)
			p.protocol.Store(1)
			a.recordPrivateReadiness(privateReadinessSnapshot{})
		}
	}
}
