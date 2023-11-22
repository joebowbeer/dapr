/*
Copyright 2021 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package placement

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"golang.org/x/exp/maps"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dapr/dapr/pkg/actors/internal"
	diag "github.com/dapr/dapr/pkg/diagnostics"
	"github.com/dapr/dapr/pkg/placement/hashing"
	v1pb "github.com/dapr/dapr/pkg/proto/placement/v1"
	"github.com/dapr/dapr/pkg/resiliency"
	"github.com/dapr/dapr/pkg/security"
	"github.com/dapr/kit/logger"
	"github.com/dapr/kit/ptr"
)

var log = logger.NewLogger("dapr.runtime.actors.placement")

const (
	lockOperation   = "lock"
	unlockOperation = "unlock"
	updateOperation = "update"

	// Interval to wait for app health's readiness
	placementReadinessWaitInterval = 500 * time.Millisecond
	// Minimum and maximum reconnection intervals
	placementReconnectMinInterval = 1 * time.Second
	placementReconnectMaxInterval = 30 * time.Second
	statusReportHeartbeatInterval = 1 * time.Second

	grpcServiceConfig = `{"loadBalancingPolicy":"round_robin"}`
)

// actorPlacement maintains membership of actor instances and consistent hash
// tables to discover the actor while interacting with Placement service.
type actorPlacement struct {
	actorTypes []string
	appID      string
	// runtimeHostname is the address and port of the runtime
	runtimeHostName string
	// name of the pod hosting the actor
	podName string

	// client is the placement client.
	client *placementClient

	// serverAddr is the list of placement addresses.
	serverAddr []string
	// serverIndex is the current index of placement servers in serverAddr.
	serverIndex atomic.Int32

	// placementTables is the consistent hashing table map to
	// look up Dapr runtime host address to locate actor.
	placementTables *hashing.ConsistentHashTables
	// placementTableLock is the lock for placementTables.
	placementTableLock sync.RWMutex
	// hasPlacementTablesCh is closed when the placement tables have been received.
	hasPlacementTablesCh chan struct{}

	// apiLevel is the current API level of the cluster
	apiLevel uint32
	// onAPILevelUpdate is invoked when the API level is updated
	onAPILevelUpdate func(apiLevel uint32)

	// unblockSignal is the channel to unblock table locking.
	unblockSignal chan struct{}
	// operationUpdateLock is the lock for three stage commit.
	operationUpdateLock sync.Mutex

	// appHealthFn returns the appHealthCh
	appHealthFn func(ctx context.Context) <-chan bool
	// appHealthy contains the result of the app health checks.
	appHealthy atomic.Bool
	// afterTableUpdateFn is function for post processing done after table updates,
	// such as draining actors and resetting reminders.
	afterTableUpdateFn func()

	// callback invoked to halt all active actors
	haltAllActorsFn internal.HaltAllActorsFn

	// shutdown is the flag when runtime is being shutdown.
	shutdown atomic.Bool
	// shutdownConnLoop is the wait group to wait until all connection loop are done
	shutdownConnLoop sync.WaitGroup
	// closeCh is the channel to close the placement service.
	closeCh chan struct{}

	resiliency resiliency.Provider
}

// ActorPlacementOpts contains options for NewActorPlacement.
type ActorPlacementOpts struct {
	ServerAddrs        []string // Address(es) for the Placement service
	Security           security.Handler
	AppID              string
	RuntimeHostname    string
	PodName            string
	ActorTypes         []string
	AppHealthFn        func(ctx context.Context) <-chan bool
	AfterTableUpdateFn func()
	Resiliency         resiliency.Provider
}

// NewActorPlacement initializes ActorPlacement for the actor service.
func NewActorPlacement(opts ActorPlacementOpts) internal.PlacementService {
	servers := addDNSResolverPrefix(opts.ServerAddrs)
	return &actorPlacement{
		actorTypes:      opts.ActorTypes,
		appID:           opts.AppID,
		runtimeHostName: opts.RuntimeHostname,
		podName:         opts.PodName,
		serverAddr:      servers,

		client:          newPlacementClient(getGrpcOptsGetter(servers, opts.Security)),
		placementTables: &hashing.ConsistentHashTables{Entries: make(map[string]*hashing.Consistent)},

		unblockSignal:      make(chan struct{}, 1),
		appHealthFn:        opts.AppHealthFn,
		afterTableUpdateFn: opts.AfterTableUpdateFn,
		closeCh:            make(chan struct{}),
		resiliency:         opts.Resiliency,
	}
}

// Register an actor type by adding it to the list of known actor types (if it's not already registered)
// The placement tables will get updated when the next heartbeat fires
func (p *actorPlacement) AddHostedActorType(actorType string, idleTimeout time.Duration) error {
	for _, t := range p.actorTypes {
		if t == actorType {
			return fmt.Errorf("actor type %s already registered", actorType)
		}
	}

	p.actorTypes = append(p.actorTypes, actorType)
	return nil
}

// Start connects placement service to register to membership and send heartbeat
// to report the current member status periodically.
func (p *actorPlacement) Start(ctx context.Context) error {
	p.serverIndex.Store(0)
	p.shutdown.Store(false)
	p.appHealthy.Store(true)
	p.resetPlacementTables()

	if !p.establishStreamConn(ctx) {
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	p.shutdownConnLoop.Add(1)
	go func() {
		defer p.shutdownConnLoop.Done()

		select {
		case <-ctx.Done():
		case <-p.closeCh:
		}
		cancel()
	}()

	p.shutdownConnLoop.Add(1)
	go func() {
		defer p.shutdownConnLoop.Done()
		ch := p.appHealthFn(ctx)
		if ch == nil {
			return
		}

		for healthy := range ch {
			p.appHealthy.Store(healthy)
		}
	}()

	// establish connection loop, whenever a disconnect occurs it starts to run trying to connect to a new server.
	p.shutdownConnLoop.Add(1)
	go func() {
		defer p.shutdownConnLoop.Done()
		for !p.shutdown.Load() {
			// wait until disconnection occurs or shutdown is triggered
			p.client.waitUntil(func(streamConnAlive bool) bool {
				return !streamConnAlive || p.shutdown.Load()
			})

			if p.shutdown.Load() {
				break
			}
			p.establishStreamConn(ctx)
		}
	}()

	// Establish receive channel to retrieve placement table update
	p.shutdownConnLoop.Add(1)
	go func() {
		defer p.shutdownConnLoop.Done()
		for !p.shutdown.Load() {
			// Wait until stream is connected or shutdown is triggered.
			p.client.waitUntil(func(streamAlive bool) bool {
				return streamAlive || p.shutdown.Load()
			})

			resp, err := p.client.recv()
			if p.shutdown.Load() {
				break
			}

			// TODO: we may need to handle specific errors later.
			if err != nil {
				p.client.disconnectFn(func() {
					p.onPlacementError(err) // depending on the returned error a new server could be used
				})
			} else {
				p.onPlacementOrder(resp)
			}
		}
	}()

	// Send the current host status to placement to register the member and
	// maintain the status of member by placement.
	p.shutdownConnLoop.Add(1)
	go func() {
		defer p.shutdownConnLoop.Done()
		for !p.shutdown.Load() {
			// Wait until stream is connected or shutdown is triggered.
			p.client.waitUntil(func(streamAlive bool) bool {
				return streamAlive || p.shutdown.Load()
			})

			if p.shutdown.Load() {
				break
			}

			// appHealthy is the health status of actor service application. This allows placement to update
			// the member list and hashing table quickly.
			if !p.appHealthy.Load() {
				// app is unresponsive, close the stream and disconnect from the placement service.
				// Then Placement will remove this host from the member list.
				log.Debug("Disconnecting from placement service by the unhealthy app")

				p.client.disconnect()
				p.placementTableLock.Lock()
				p.resetPlacementTables()
				p.placementTableLock.Unlock()
				if p.haltAllActorsFn != nil {
					haltErr := p.haltAllActorsFn()
					if haltErr != nil {
						log.Errorf("Failed to deactivate all actors: %v", haltErr)
					}
				}
				continue
			}

			host := v1pb.Host{
				Name:     p.runtimeHostName,
				Entities: p.actorTypes,
				Id:       p.appID,
				Load:     1, // Not used yet
				Pod:      p.podName,
				// Port is redundant because Name should include port number
				// Port: 0,
				ApiLevel: internal.ActorAPILevel,
			}

			err := p.client.send(&host)
			if err != nil {
				diag.DefaultMonitoring.ActorStatusReportFailed("send", "status")
				log.Errorf("Failed to report status to placement service : %v", err)
			}

			// No delay if stream connection is not alive.
			if p.client.isConnected() {
				diag.DefaultMonitoring.ActorStatusReported("send")
				time.Sleep(statusReportHeartbeatInterval)
			}
		}
	}()

	return nil
}

// Closes shuts down server stream gracefully.
func (p *actorPlacement) Close() error {
	// CAS to avoid stop more than once.
	if p.shutdown.CompareAndSwap(false, true) {
		p.client.disconnect()
		p.shutdown.Store(true)
		close(p.closeCh)
	}
	p.shutdownConnLoop.Wait()
	return nil
}

// WaitUntilReady waits until placement table is until table lock is unlocked.
func (p *actorPlacement) WaitUntilReady(ctx context.Context) error {
	p.placementTableLock.RLock()
	hasTablesCh := p.hasPlacementTablesCh
	p.placementTableLock.RUnlock()

	select {
	case p.unblockSignal <- struct{}{}:
		select {
		case <-p.unblockSignal:
		default:
		}
		// continue
	case <-ctx.Done():
		return ctx.Err()
	}

	if hasTablesCh == nil {
		return nil
	}

	select {
	case <-hasTablesCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// LookupActor resolves to actor service instance address using consistent hashing table.
func (p *actorPlacement) LookupActor(ctx context.Context, req internal.LookupActorRequest) (internal.LookupActorResponse, error) {
	// Retry here to allow placement table dissemination/rebalancing to happen.
	policyDef := p.resiliency.BuiltInPolicy(resiliency.BuiltInActorNotFoundRetries)
	policyRunner := resiliency.NewRunner[internal.LookupActorResponse](ctx, policyDef)
	return policyRunner(func(ctx context.Context) (res internal.LookupActorResponse, rErr error) {
		rAddr, rAppID, rErr := p.doLookupActor(ctx, req.ActorType, req.ActorID)
		if rErr != nil {
			return res, fmt.Errorf("error finding address for actor %s/%s: %w", req.ActorType, req.ActorID, rErr)
		} else if rAddr == "" {
			return res, fmt.Errorf("did not find address for actor %s/%s", req.ActorType, req.ActorID)
		}
		res.Address = rAddr
		res.AppID = rAppID
		return res, nil
	})
}

func (p *actorPlacement) doLookupActor(ctx context.Context, actorType, actorID string) (string, string, error) {
	p.placementTableLock.RLock()
	defer p.placementTableLock.RUnlock()

	if p.placementTables == nil {
		return "", "", errors.New("placement tables are not set")
	}

	t := p.placementTables.Entries[actorType]
	if t == nil {
		return "", "", nil
	}
	host, err := t.GetHost(actorID)
	if err != nil || host == nil {
		return "", "", nil //nolint:nilerr
	}
	return host.Name, host.AppID, nil
}

//nolint:nosnakecase
func (p *actorPlacement) establishStreamConn(ctx context.Context) (established bool) {
	// Backoff for reconnecting in case of errors
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = placementReconnectMinInterval
	bo.MaxInterval = placementReconnectMaxInterval
	bo.MaxElapsedTime = 0 // Retry forever

	logFailureShown := false
	for !p.shutdown.Load() {
		// Do not retry to connect if context is canceled
		if ctx.Err() != nil {
			return false
		}

		// Stop reconnecting to placement until app is healthy.
		if !p.appHealthy.Load() {
			// We are not using an exponential backoff here because we haven't begun to establish connections yet
			time.Sleep(placementReadinessWaitInterval)
			continue
		}

		// Check for context validity again, after sleeping
		if ctx.Err() != nil {
			return false
		}

		serverAddr := p.serverAddr[p.serverIndex.Load()]

		if !logFailureShown {
			log.Debug("try to connect to placement service: " + serverAddr)
		}

		err := p.client.connectToServer(ctx, serverAddr)
		if err == errEstablishingTLSConn {
			return false
		}

		if err != nil {
			if !logFailureShown {
				log.Debugf("Error connecting to placement service (will retry to connect in background): %v", err)
				// Don't show the debug log more than once per each reconnection attempt
				logFailureShown = true
			}

			// Try a different instance of the placement service
			p.serverIndex.Store((p.serverIndex.Load() + 1) % int32(len(p.serverAddr)))

			// Halt all active actors, then reset the placement tables
			if p.haltAllActorsFn != nil {
				p.haltAllActorsFn()
			}
			p.resetPlacementTables()

			// Sleep with an exponential backoff
			select {
			case <-time.After(bo.NextBackOff()):
			case <-ctx.Done():
				return false
			}
			continue
		}

		log.Debug("Established connection to placement service at " + p.client.clientConn.Target())
		return true
	}

	return false
}

// onPlacementError closes the current placement stream and reestablish the connection again,
// uses a different placement server depending on the error code
func (p *actorPlacement) onPlacementError(err error) {
	s, ok := status.FromError(err)
	// If the current server is not leader, then it will try to the next server.
	if ok && s.Code() == codes.FailedPrecondition {
		p.serverIndex.Store((p.serverIndex.Load() + 1) % int32(len(p.serverAddr)))
	} else {
		log.Debugf("Disconnected from placement: %v", err)
	}
}

func (p *actorPlacement) onPlacementOrder(in *v1pb.PlacementOrder) {
	log.Debugf("Placement order received: %s", in.Operation)
	diag.DefaultMonitoring.ActorPlacementTableOperationReceived(in.Operation)

	// lock all incoming calls when an updated table arrives
	p.operationUpdateLock.Lock()
	defer p.operationUpdateLock.Unlock()

	switch in.Operation {
	case lockOperation:
		p.blockPlacements()

		go func() {
			// TODO: Use lock-free table update.
			// current implementation is distributed two-phase locking algorithm.
			// If placement experiences intermittently outage during updateplacement,
			// user application will face 5 second blocking even if it can avoid deadlock.
			// It can impact the entire system.
			time.Sleep(time.Second * 5)
			p.unblockPlacements()
		}()

	case unlockOperation:
		p.unblockPlacements()

	case updateOperation:
		p.updatePlacements(in.Tables)
	}
}

func (p *actorPlacement) blockPlacements() {
	select {
	case p.unblockSignal <- struct{}{}:
		// Now  blocked
	default:
		// Was already blocked
	}
}

func (p *actorPlacement) unblockPlacements() {
	select {
	case <-p.unblockSignal:
		// Now unblocked
	default:
		// Was already unblocked
	}
}

// Resets the placement tables.
// Note that this method should be invoked by a caller that owns a lock.
func (p *actorPlacement) resetPlacementTables() {
	if p.hasPlacementTablesCh != nil {
		close(p.hasPlacementTablesCh)
	}
	p.hasPlacementTablesCh = make(chan struct{})
	maps.Clear(p.placementTables.Entries)
	p.placementTables.Version = ""
}

func (p *actorPlacement) updatePlacements(in *v1pb.PlacementTables) {
	updated := false
	var updatedAPILevel *uint32
	func() {
		p.placementTableLock.Lock()
		defer p.placementTableLock.Unlock()

		if in.Version == p.placementTables.Version {
			return
		}

		if in.ApiLevel != p.apiLevel {
			p.apiLevel = in.ApiLevel
			updatedAPILevel = ptr.Of(in.ApiLevel)
		}

		maps.Clear(p.placementTables.Entries)
		p.placementTables.Version = in.Version
		for k, v := range in.Entries {
			loadMap := make(map[string]*hashing.Host, len(v.LoadMap))
			for lk, lv := range v.LoadMap {
				loadMap[lk] = hashing.NewHost(lv.Name, lv.Id, lv.Load, lv.Port)
			}
			p.placementTables.Entries[k] = hashing.NewFromExisting(v.Hosts, v.SortedSet, loadMap)
		}

		updated = true
		if p.hasPlacementTablesCh != nil {
			close(p.hasPlacementTablesCh)
			p.hasPlacementTablesCh = nil
		}
	}()

	if updatedAPILevel != nil && p.onAPILevelUpdate != nil {
		p.onAPILevelUpdate(*updatedAPILevel)
	}

	if updated {
		// May call LookupActor inside, so should not do this with placementTableLock locked.
		if p.afterTableUpdateFn != nil {
			p.afterTableUpdateFn()
		}
		log.Infof("Placement tables updated, version: %s", in.GetVersion())
	}
}

func (p *actorPlacement) SetOnAPILevelUpdate(fn func(apiLevel uint32)) {
	p.onAPILevelUpdate = fn
}

func (p *actorPlacement) ReportActorDeactivation(ctx context.Context, actorType, actorID string) error {
	// Nop in this implementation
	return nil
}

func (p *actorPlacement) SetHaltActorFns(haltFn internal.HaltActorFn, haltAllFn internal.HaltAllActorsFn) {
	// haltFn isn't used in this implementation
	p.haltAllActorsFn = haltAllFn
	return
}

// addDNSResolverPrefix add the `dns://` prefix to the given addresses
func addDNSResolverPrefix(addr []string) []string {
	resolvers := make([]string, 0, len(addr))
	for _, a := range addr {
		prefix := ""
		host, _, err := net.SplitHostPort(a)
		if err == nil && net.ParseIP(host) == nil {
			prefix = "dns:///"
		}
		resolvers = append(resolvers, prefix+a)
	}
	return resolvers
}