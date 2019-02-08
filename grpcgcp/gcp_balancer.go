/*
 *
 * Copyright 2018 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package grpcgcp

import (
	"sync"
	"sync/atomic"

	"google.golang.org/grpc/balancer"

	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/resolver"
)

const (
	// Name is the name of grpc_gcp balancer.
	Name = "grpc_gcp"

	// Default settings for max pool size and max concurrent streams.
	defaultMaxConn   = 10
	defaultMaxStream = 100

	healthCheckEnabled = true
)

func init() {
	balancer.Register(newBuilder())
}

type gcpBalancerBuilder struct {
	name string
}

func (bb *gcpBalancerBuilder) Build(
	cc balancer.ClientConn,
	opt balancer.BuildOptions,
) balancer.Balancer {
	return &gcpBalancer{
		cc:          cc,
		affinityMap: make(map[string]balancer.SubConn),
		scRefs:      make(map[balancer.SubConn]*subConnRef),
		scStates:    make(map[balancer.SubConn]connectivity.State),
		csEvltr:     &ConnectivityStateEvaluator{},
		// Initialize picker to a picker that always return
		// ErrNoSubConnAvailable, because when state of a SubConn changes, we
		// may call UpdateBalancerState with this picker.
		picker: newErrPicker(balancer.ErrNoSubConnAvailable),
	}
}

func (*gcpBalancerBuilder) Name() string {
	return Name
}

// newBuilder creates a new grpcgcp balancer builder.
func newBuilder() balancer.Builder {
	return &gcpBalancerBuilder{
		name: Name,
	}
}

// ConnectivityStateEvaluator gets updated by addrConns when their
// states transition, based on which it evaluates the state of
// ClientConn.
type ConnectivityStateEvaluator struct {
	numReady            uint64 // Number of addrConns in ready state.
	numConnecting       uint64 // Number of addrConns in connecting state.
	numTransientFailure uint64 // Number of addrConns in transientFailure.
}

// RecordTransition records state change happening in every subConn and based on
// that it evaluates what aggregated state should be.
// It can only transition between Ready, Connecting and TransientFailure. Other states,
// Idle and Shutdown are transitioned into by ClientConn; in the beginning of the connection
// before any subConn is created ClientConn is in idle state. In the end when ClientConn
// closes it is in Shutdown state.
//
// recordTransition should only be called synchronously from the same goroutine.
func (cse *ConnectivityStateEvaluator) RecordTransition(
	oldState,
	newState connectivity.State,
) connectivity.State {
	// Update counters.
	for idx, state := range []connectivity.State{oldState, newState} {
		updateVal := 2*uint64(idx) - 1 // -1 for oldState and +1 for new.
		switch state {
		case connectivity.Ready:
			cse.numReady += updateVal
		case connectivity.Connecting:
			cse.numConnecting += updateVal
		case connectivity.TransientFailure:
			cse.numTransientFailure += updateVal
		}
	}

	// Evaluate.
	if cse.numReady > 0 {
		return connectivity.Ready
	}
	if cse.numConnecting > 0 {
		return connectivity.Connecting
	}
	return connectivity.TransientFailure
}

// subConnRef keeps reference to the real SubConn with its
// connectivity state, affinity count and streams count.
type subConnRef struct {
	subConn     balancer.SubConn
	affinityCnt int32 // Keeps track of the number of keys bound to the subConn
	streamsCnt  int32 // Keeps track of the number of streams opened on the subConn
}

func (ref *subConnRef) affinityIncr() {
	atomic.AddInt32(&ref.affinityCnt, 1)
}

func (ref *subConnRef) affinityDecr() {
	atomic.AddInt32(&ref.affinityCnt, -1)
}

func (ref *subConnRef) streamsIncr() {
	atomic.AddInt32(&ref.streamsCnt, 1)
}

func (ref *subConnRef) streamsDecr() {
	atomic.AddInt32(&ref.streamsCnt, -1)
}

type gcpBalancer struct {
	addrs   []resolver.Address
	cc      balancer.ClientConn
	csEvltr *ConnectivityStateEvaluator
	state   connectivity.State

	mu          sync.Mutex
	affinityMap map[string]balancer.SubConn
	scStates    map[balancer.SubConn]connectivity.State
	scRefs      map[balancer.SubConn]*subConnRef

	picker balancer.Picker
}

func (gb *gcpBalancer) HandleResolvedAddrs(addrs []resolver.Address, err error) {
	if err != nil {
		grpclog.Infof(
			"grpcgcp.gcpBalancer: HandleResolvedAddrs called with error %v",
			err,
		)
		return
	}
	grpclog.Infoln("grpcgcp.gcpBalancer: got new resolved addresses: ", addrs)
	gb.addrs = addrs

	if len(gb.scRefs) == 0 {
		gb.newSubConn()
		return
	}

	for _, scRef := range gb.scRefs {
		// TODO(weiranf): update streams count when new addrs resolved?
		scRef.subConn.UpdateAddresses(addrs)
		scRef.subConn.Connect()
	}
}

// check current connection pool size
func (gb *gcpBalancer) getConnectionPoolSize() int {
	gb.mu.Lock()
	defer gb.mu.Unlock()
	return len(gb.scRefs)
}

// newSubConn creates a new SubConn using cc.NewSubConn and initialize the subConnRef.
func (gb *gcpBalancer) newSubConn() {
	gb.mu.Lock()
	defer gb.mu.Unlock()

	// there are chances the newly created subconns are still connecting,
	// we can wait on those new subconns.
	for _, scState := range gb.scStates {
		if scState == connectivity.Connecting {
			return
		}
	}

	sc, err := gb.cc.NewSubConn(
		gb.addrs,
		balancer.NewSubConnOptions{HealthCheckEnabled: healthCheckEnabled},
	)
	if err != nil {
		grpclog.Errorf("grpcgcp.gcpBalancer: failed to NewSubConn: %v", err)
		return
	}
	gb.scRefs[sc] = &subConnRef{
		subConn: sc,
	}
	gb.scStates[sc] = connectivity.Idle
	sc.Connect()
}

// getReadySubConnRef returns a subConnRef and a bool. The bool indicates whether
// the boundKey exists in the affinityMap. If returned subConnRef is a nil, it
// means the underlying subconn is not READY yet.
func (gb *gcpBalancer) getReadySubConnRef(boundKey string) (*subConnRef, bool) {
	gb.mu.Lock()
	defer gb.mu.Unlock()

	if sc, ok := gb.affinityMap[boundKey]; ok {
		if gb.scStates[sc] != connectivity.Ready {
			// It's possible that the bound subconn is not in the readySubConns list,
			// If it's not ready yet, we throw ErrNoSubConnAvailable
			return nil, true
		}
		return gb.scRefs[sc], true
	}
	return nil, false
}

// bindSubConn binds the given affinity key to an existing subConnRef.
func (gb *gcpBalancer) bindSubConn(bindKey string, sc balancer.SubConn) {
	gb.mu.Lock()
	defer gb.mu.Unlock()
	_, ok := gb.affinityMap[bindKey]
	if !ok {
		gb.affinityMap[bindKey] = sc
	}
	gb.scRefs[sc].affinityIncr()
}

// unbindSubConn removes the existing binding associated with the key.
func (gb *gcpBalancer) unbindSubConn(boundKey string) {
	gb.mu.Lock()
	defer gb.mu.Unlock()
	boundSC, ok := gb.affinityMap[boundKey]
	if ok {
		gb.scRefs[boundSC].affinityDecr()
		if gb.scRefs[boundSC].affinityCnt <= 0 {
			delete(gb.affinityMap, boundKey)
		}
	}
}

// regeneratePicker takes a snapshot of the balancer, and generates a picker
// from it. The picker is
//  - errPicker with ErrTransientFailure if the balancer is in TransientFailure,
//  - built by the pickerBuilder with all READY SubConns otherwise.
func (gb *gcpBalancer) regeneratePicker() {
	if gb.state == connectivity.TransientFailure {
		gb.picker = newErrPicker(balancer.ErrTransientFailure)
		return
	}
	readyRefs := []*subConnRef{}

	// Select ready subConns from subConn map.
	for sc, scState := range gb.scStates {
		if scState == connectivity.Ready {
			readyRefs = append(readyRefs, gb.scRefs[sc])
		}
	}
	gb.picker = newGCPPicker(readyRefs, gb)
}

func (gb *gcpBalancer) HandleSubConnStateChange(sc balancer.SubConn, s connectivity.State) {
	grpclog.Infof("grpcgcp.gcpBalancer: handle SubConn state change: %p, %v", sc, s)

	gb.mu.Lock()
	oldS, ok := gb.scStates[sc]
	if !ok {
		grpclog.Infof(
			"grpcgcp.gcpBalancer: got state changes for an unknown SubConn: %p, %v",
			sc,
			s,
		)
		gb.mu.Unlock()
		return
	}
	gb.scStates[sc] = s
	switch s {
	case connectivity.Idle:
		sc.Connect()
	case connectivity.Shutdown:
		delete(gb.scRefs, sc)
		delete(gb.scStates, sc)
	}
	gb.mu.Unlock()

	oldAggrState := gb.state
	gb.state = gb.csEvltr.RecordTransition(oldS, s)

	// Regenerate picker when one of the following happens:
	//  - this sc became ready from not-ready
	//  - this sc became not-ready from ready
	//  - the aggregated state of balancer became TransientFailure from non-TransientFailure
	//  - the aggregated state of balancer became non-TransientFailure from TransientFailure
	if (s == connectivity.Ready) != (oldS == connectivity.Ready) ||
		(gb.state == connectivity.TransientFailure) != (oldAggrState == connectivity.TransientFailure) {
		gb.regeneratePicker()
		gb.cc.UpdateBalancerState(gb.state, gb.picker)
	}
}

func (gb *gcpBalancer) Close() {
}