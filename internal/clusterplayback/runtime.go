package clusterplayback

import (
	"context"
	"crypto/tls"
	"database/sql"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nodeauthority"

	"github.com/braedonsaunders/ether/internal/nodesource"
	"github.com/braedonsaunders/ether/internal/nodewire"
	"github.com/braedonsaunders/ether/internal/nodewire/outbound"
	"github.com/braedonsaunders/ether/internal/placement"
	"github.com/braedonsaunders/ether/internal/playback"
	"github.com/braedonsaunders/ether/internal/playbackbuffer"
	"github.com/braedonsaunders/ether/internal/remotetranscode"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const SourcePath = "/cluster/playback/source"
const WorkerPath = "/cluster/playback/worker"

type Options struct {
	// OutputBuffer is an opt-in surviving-controller segment store. Scratch has
	// one slot of MaxSegmentBytes; nil leaves existing delivery unchanged.
	OutputBuffer                           *playbackbuffer.Store
	MaxSegmentBytes                        int64
	WorkerCapabilities, RemoteCapabilities remotetranscode.Capabilities
	// HistoryCapacity bounds boot-retained authority tombstones (default4096),
	// independently of concurrent placement resource capacity.
	HistoryCapacity int
	// SourcePolicy rechecks live viewer/title/source rights from local facts.
	SourcePolicy func(context.Context, *core.Viewer, *core.PlaySession) error
	MemberRole   func(context.Context, nodewire.Identity, config.NodeRole) error
	// Eligible checks live exact boot membership, role, protocol and drain policy.
	Eligible        func(context.Context, nodewire.Identity, config.NodeRole) error
	DB              *sql.DB
	Guard           *nodewire.Guard
	Self            nodewire.Identity
	Roles           config.NodeRoles
	Placement       *placement.Service
	Endpoint        func(context.Context, nodewire.Identity) (string, error)
	ActiveViewer    func(context.Context, int64) (*core.Viewer, error)
	Playback        *playback.Manager
	SourceAccess    *playback.SourceAccess
	Encoder         core.OwnedTranscoder
	Decide          func(core.MediaInfo, core.ClientCaps, core.DecideOptions) core.Decision
	Hardware        core.HWAccel
	Capacity        int
	ClockSkew       time.Duration
	ClockBoundKnown bool
}
type Runtime struct {
	outputScratch  chan struct{}
	history        *placement.History
	credentials    map[string]sourceCredential
	reopenLives    map[string]*ReopenResponsibility
	publicDelivery bool
	reopens        map[string]*reopenRecord
	plexDelivery   http.Handler
	remotePlayback *RemotePlayback
	control        sync.Mutex
	preparations   map[string]*preparation
	delivery       func(http.ResponseWriter, *http.Request, string)
	workerStarting int
	encoderTokens  []core.EncoderRetirement
	o              Options
	mu             sync.Mutex
	closed         bool
	mux            *http.ServeMux
	authority      *nodeauthority.Authority
	bytes          *nodesource.Handler
	sources        map[string]*ownedSource
	worker         *remotetranscode.Worker
	inputs         map[remotetranscode.Identity]*workerInput
	executions     map[string]*execution
}

func NewRuntime(o Options) (*Runtime, error) {
	if o.OutputBuffer != nil && (o.MaxSegmentBytes <= 0 || o.MaxSegmentBytes > 64<<20) {
		return nil, ErrDenied
	}
	var capErr error
	o.WorkerCapabilities, capErr = remotetranscode.NormalizeCapabilities(o.WorkerCapabilities)
	if capErr != nil {
		return nil, capErr
	}
	o.RemoteCapabilities, capErr = remotetranscode.NormalizeCapabilities(o.RemoteCapabilities)
	if capErr != nil {
		return nil, capErr
	}
	if o.HistoryCapacity == 0 {
		o.HistoryCapacity = 4096
	}
	if o.HistoryCapacity < o.Capacity || o.HistoryCapacity > 4096 {
		return nil, ErrDenied
	}
	if o.DB == nil || o.Guard == nil || o.Placement == nil || o.Endpoint == nil || o.Eligible == nil || o.MemberRole == nil || o.Self.Cluster == "" || o.Self.NodeID == "" || o.Self.Boot == "" || !o.ClockBoundKnown || o.ClockSkew < 0 || o.ClockSkew > time.Minute || o.Capacity < 1 || o.Capacity > 4096 {
		return nil, ErrDenied
	}
	r := &Runtime{credentials: map[string]sourceCredential{}, reopenLives: map[string]*ReopenResponsibility{}, reopens: map[string]*reopenRecord{}, preparations: map[string]*preparation{}, o: o, mux: http.NewServeMux(), sources: map[string]*ownedSource{}, inputs: map[remotetranscode.Identity]*workerInput{}, executions: map[string]*execution{}}
	if o.OutputBuffer != nil {
		r.outputScratch = make(chan struct{}, 1)
	}
	history, historyErr := placement.NewHistory(o.Placement, o.Self.Cluster, r)
	if historyErr != nil {
		return nil, historyErr
	}
	r.history = history
	if o.Playback != nil {
		if o.SourceAccess == nil || o.ActiveViewer == nil || o.SourcePolicy == nil {
			return nil, ErrDenied
		}
		a, e := nodeauthority.New(nodeauthority.Options{Lookup: o.Placement, Policy: r.sourcePolicy, Owner: placement.Target{NodeID: o.Self.NodeID, Boot: o.Self.Boot}, Now: time.Now, ClockBoundKnown: true, ClockSkew: o.ClockSkew, MaxLifetime: 10 * time.Minute, MaxBindings: o.HistoryCapacity, MaxGrants: 4096, MaxGrantsPerBinding: 4})
		if e != nil {
			return nil, e
		}
		r.authority = a
		h, e := nodesource.NewHandler(nodesource.Options{Authority: trackedAuthority{r: r}, Peer: o.Guard.SourcePeer, Acquire: r.acquireSource, Now: time.Now, MaxRequests: o.Capacity, MaxLifetime: 10 * time.Minute, Revalidate: time.Second})
		if e != nil {
			return nil, e
		}
		r.bytes = h
		r.mux.Handle(nodesource.Path, h)
		r.mux.HandleFunc(PreparedBytesPath, r.preparedBytes)
		r.mux.HandleFunc(SourcePath, r.sourceHTTP)
	}
	if o.Encoder != nil {
		w, e := remotetranscode.NewWorker(remotetranscode.WorkerOptions{Capabilities: o.WorkerCapabilities, Boot: o.Self.Boot, Capacity: o.HistoryCapacity, MaxLease: 10 * time.Minute, Encoder: recordingEncoder{r: r}, Security: remotetranscode.Security{Authenticate: o.Guard.RemoteAuthenticate, Authorize: r.authorizeWorker}, Resolve: r.resolveInput, ValidateRenewal: r.renewInput})
		if e != nil {
			return nil, e
		}
		r.mux.HandleFunc(CapabilitiesPath, r.capabilitiesHTTP)
		r.worker = w
		r.mux.Handle(WorkerPath, w)
	}
	r.mux.HandleFunc(DeliveryPath, r.deliveryHTTP)
	r.mux.HandleFunc(TerminalPath, r.terminalHTTP)
	r.mux.HandleFunc(EvidencePath, r.evidenceHTTP)
	r.mux.HandleFunc(PlexDeliveryPath, r.plexDeliveryHTTP)
	return r, nil
}
func (r *Runtime) Handler() http.Handler { return r.mux }
func (r *Runtime) Ready() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	if r.o.Roles.Has(config.RoleSource) && r.authority == nil {
		return false
	}
	if r.o.Roles.Has(config.RoleTranscode) && r.worker == nil {
		return false
	}
	if r.o.Roles.Has(config.RoleDelivery) {
		if r.o.Roles.Has(config.RoleServer) {
			if r.delivery == nil {
				return false
			}
		} else if !r.publicDelivery {
			return false
		}
	}
	return r.worker != nil || r.authority != nil || r.delivery != nil || r.publicDelivery
}
func (r *Runtime) peer(ctx context.Context, id nodewire.Identity, path string) (string, *tls.Config, func(context.Context, tls.ConnectionState) error, error) {
	origin, e := r.o.Endpoint(ctx, id)
	if e != nil {
		return "", nil, nil, ErrDenied
	}
	u, e := url.Parse(origin)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", nil, nil, ErrDenied
	}
	tc, e := r.o.Guard.ClientTLS(id, u.Hostname())
	if e != nil {
		return "", nil, nil, e
	}
	check := func(c context.Context, s tls.ConnectionState) error {
		return r.o.Guard.RecheckServer(c, s, id, u.Hostname())
	}
	return "https://" + u.Host + path, tc, check, nil
}
func (r *Runtime) controlClient(ctx context.Context, id nodewire.Identity) (*http.Client, string, error) {
	ep, tc, check, e := r.peer(ctx, id, SourcePath)
	if e != nil {
		return nil, "", e
	}
	t, e := outbound.New(ep, tc, 5*time.Second, check)
	if e != nil {
		return nil, "", e
	}
	return &http.Client{Transport: t, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, ep, nil
}

// Cleanup may block in backend Close. Never run it on the lease/control executor.
func (r *Runtime) Cleanup() {
	r.mu.Lock()
	list := make([]*workerInput, 0, len(r.inputs))
	for _, v := range r.inputs {
		list = append(list, v)
	}
	r.mu.Unlock()
	for _, v := range list {
		v.gateway.Reconcile()
	}
	if r.bytes != nil {
		r.bytes.Reconcile()
	}
	if r.o.SourceAccess != nil {
		r.o.SourceAccess.Reconcile()
	}
}

func (r *Runtime) Close() error {
	r.mu.Lock()
	r.closed = true
	for _, life := range r.reopenLives {
		life.retiring = true
	}
	list := make([]*execution, 0, len(r.executions))
	for _, x := range r.executions {
		list = append(list, x)
	}
	inputs := make([]*workerInput, 0, len(r.inputs))
	for _, x := range r.inputs {
		inputs = append(inputs, x)
	}
	r.mu.Unlock()
	r.mu.Lock()
	remote := r.remotePlayback
	r.mu.Unlock()
	if remote != nil {
		remote.close()
	}
	if r.worker != nil {
		_ = r.worker.Close()
	}
	for _, x := range list {
		r.Stop(x.route.Fence.Session)
	}
	for _, x := range inputs {
		x.cancel()
		_ = x.gateway.Close()
		<-x.done
		x.client.Close()
	}
	return nil
}
func (r *Runtime) sourcePolicy(ctx context.Context, owner placement.Target, s nodesource.Scope) error {
	route, e := r.route(ctx, s.Fence.Session)
	if e != nil || route.Fence != s.Fence || route.Source.NodeID != owner.NodeID || route.Source.Boot != owner.Boot || route.Revision != s.Revision || viewerString(route.Viewer) != s.Viewer {
		return ErrDenied
	}
	for _, pair := range []struct {
		id   nodewire.Identity
		role config.NodeRole
	}{
		{route.Source, config.RoleSource},
		{nodewire.Identity{Cluster: r.o.Self.Cluster, NodeID: s.Fence.Controller.ID, Boot: s.Fence.Controller.Boot}, config.RoleServer},
		{nodewire.Identity{Cluster: r.o.Self.Cluster, NodeID: s.Fence.Target.NodeID, Boot: s.Fence.Target.Boot}, config.RoleTranscode},
	} {
		if r.o.MemberRole(ctx, pair.id, pair.role) != nil {
			return ErrDenied
		}
	}
	v, e := r.o.ActiveViewer(ctx, route.Viewer)
	if e != nil || v == nil || v.ID != route.Viewer {
		return ErrDenied
	}
	ps, ok := r.o.Playback.Session(route.PlayID)
	if !ok || ps == nil || ps.UserID != route.Viewer || r.o.SourcePolicy(ctx, v, ps) != nil {
		return ErrDenied
	}
	return nil
}

// PendingDetails is a secret-free barrier inventory for drain diagnostics.
// Categories can describe the same playback; totals are not unique sessions.
// Errors or any outstanding category must keep drain blocked.
func (r *Runtime) PendingDetails(ctx context.Context) (ResponsibilityCounts, error) {
	publicationErr := r.persistPreparationReceipts(ctx)
	r.mu.Lock()
	sources := make([]ownedSource, 0, len(r.sources))
	for _, s := range r.sources {
		sources = append(sources, *s)
	}
	inputs := make([]workerInput, 0, len(r.inputs))
	for _, v := range r.inputs {
		inputs = append(inputs, *v)
	}
	r.mu.Unlock()
	r.mu.Lock()
	tokens := append([]core.EncoderRetirement(nil), r.encoderTokens...)
	starting := r.workerStarting
	r.mu.Unlock()
	r.mu.Lock()
	preps := make([]preparation, 0, len(r.preparations))
	for _, entry := range r.preparations {
		preps = append(preps, *entry)
	}
	r.mu.Unlock()
	r.mu.Lock()
	remote := r.remotePlayback
	r.mu.Unlock()
	r.mu.Lock()
	executionPending := 0
	for _, x := range r.executions {
		if !x.retired {
			executionPending++
		}
	}
	reopenPending := 0
	for _, life := range r.reopenLives {
		if !life.retired {
			reopenPending++
		}
	}
	r.mu.Unlock()
	counts := ResponsibilityCounts{EncoderStarts: starting, Reopens: reopenPending, Executions: executionPending}
	if remote != nil {
		counts.RemotePreparing, counts.RemoteLive, counts.RemoteRetiring = remote.responsibilities()
	}
	for _, entry := range preps {
		if !entry.receipt {
			counts.PreparationReceipts++
		}
		counts.SourceCallbacks += entry.operations
		if entry.running {
			counts.SourcePreparations++
		}
		if entry.life != nil {
			select {
			case <-entry.life.Done():
				if entry.life.Err() != nil {
					counts.PreparationLifetimes++
				}
			default:
				counts.PreparationLifetimes++
			}
		}
	}
	for _, token := range tokens {
		if token.EncoderErr() != nil {
			counts.EncoderTokens++
		}
	}
	for _, s := range sources {
		counts.SourceCallbacks += s.operations
		if s.binding {
			counts.SourceCallbacks++
		}
		if s.issuing && s.operations == 0 {
			counts.SourceCallbacks++
		}
		if s.life == nil {
			counts.SourceLifetimes++
			continue
		}
		select {
		case <-s.life.Done():
			if s.life.Err() != nil {
				counts.SourceLifetimes++
			}
		default:
			counts.SourceLifetimes++
		}
	}
	for _, v := range inputs {
		if v.input == nil {
			counts.Inputs++
			continue
		}
		select {
		case <-v.input.InputDone():
			if v.input.InputErr() != nil {
				counts.Inputs++
			}
		default:
			counts.Inputs++
		}
	}
	var reserved int
	err := r.o.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM placement_sessions WHERE retired=0 AND ((controller=? AND controller_boot=?) OR (node=? AND boot=?))`, r.o.Self.NodeID, r.o.Self.Boot, r.o.Self.NodeID, r.o.Self.Boot).Scan(&reserved)
	counts.Placements = reserved
	if publicationErr != nil {
		return counts, publicationErr
	}
	return counts, err
}

type recordingEncoder struct{ r *Runtime }

func (e recordingEncoder) StartHLSOwned(ctx context.Context, q core.HLSRequest) (core.HLSSession, core.EncoderRetirement, error) {
	e.r.mu.Lock()
	e.r.workerStarting++
	e.r.mu.Unlock()
	s, t, err := e.r.o.Encoder.StartHLSOwned(ctx, q)
	e.r.mu.Lock()
	e.r.workerStarting--
	if t != nil {
		e.r.encoderTokens = append(e.r.encoderTokens, t)
	}
	e.r.mu.Unlock()
	return s, t, err
}

func (e recordingEncoder) SupportsDistributedRequest(q core.HLSRequest) bool {
	backend, ok := e.r.o.Encoder.(requestSupport)
	return ok && backend.SupportsDistributedRequest(q)
}
