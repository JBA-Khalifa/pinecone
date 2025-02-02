// Copyright 2021 The Matrix.org Foundation C.I.C.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package router

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/matrix-org/pinecone/types"
	"github.com/matrix-org/pinecone/util"
)

// NOTE: Functions prefixed with an underscore (_) are only safe to be called
// from the actor that owns them, in order to prevent data races.

const virtualSnakeMaintainInterval = time.Second
const virtualSnakeNeighExpiryPeriod = time.Hour

type virtualSnakeTable map[virtualSnakeIndex]*virtualSnakeEntry

type virtualSnakeIndex struct {
	PublicKey types.PublicKey
	PathID    types.VirtualSnakePathID
}

type virtualSnakeEntry struct {
	*virtualSnakeIndex
	Origin        types.PublicKey
	Source        *peer
	Destination   *peer
	LastSeen      time.Time
	RootPublicKey types.PublicKey
	RootSequence  types.Varu64
}

// valid returns true if the update hasn't expired, or false if it has. It is
// required for updates to time out eventually, in the case that paths don't get
// torn down properly for some reason.
func (e *virtualSnakeEntry) valid() bool {
	return time.Since(e.LastSeen) < virtualSnakeNeighExpiryPeriod
}

// _maintainSnake is responsible for working out if we need to send bootstraps
// or to clean up any old paths.
func (s *state) _maintainSnake() {
	select {
	case <-s.r.context.Done():
		return
	default:
		defer s._maintainSnakeIn(virtualSnakeMaintainInterval)
	}

	// Work out if we are able to bootstrap. If we are the root node then
	// we don't send bootstraps, since there's nowhere for them to go —
	// bootstraps are sent up to the next ascending node, but as the root,
	// we already have the highest key on the network.
	rootAnn := s._rootAnnouncement()
	canBootstrap := s._parent != nil && rootAnn.RootPublicKey != s.r.public
	willBootstrap := false

	// The ascending node is the node with the next highest key.
	if asc := s._ascending; asc != nil {
		switch {
		case !asc.valid():
			// The ascending path entry has expired, so tear it down and then
			// see if we can bootstrap again.
			s._sendTeardownForExistingPath(s.r.local, asc.PublicKey, asc.PathID)
			fallthrough
		case asc.RootPublicKey != rootAnn.RootPublicKey || asc.RootSequence != rootAnn.Sequence:
			// The ascending node was set up with a different root key or sequence
			// number. In this case, we will send another bootstrap to the remote
			// side in order to hopefully replace the path with a new one.
			willBootstrap = canBootstrap
		}
	} else {
		// We don't have an ascending node at all, so if we can, we'll try
		// bootstrapping to locate it.
		willBootstrap = canBootstrap
	}

	// The descending node is the node with the next lowest key.
	if desc := s._descending; desc != nil && !desc.valid() {
		// The descending path has expired, so tear it down and then that should
		// prompt the remote side into sending a new bootstrap to set up a new
		// path, if they are still alive.
		s._sendTeardownForExistingPath(s.r.local, desc.PublicKey, desc.PathID)
	}

	// If one of the previous conditions means that we need to bootstrap, then
	// send the actual bootstrap message into the network.
	if willBootstrap {
		s._bootstrapNow()
	}
}

// _bootstrapNow is responsible for sending a bootstrap message to the network.
func (s *state) _bootstrapNow() {
	// If we are the root node then there's no point in trying to bootstrap. We
	// already have the highest public key on the network so a bootstrap won't be
	// able to go anywhere in ascending order.
	if s._parent == nil {
		return
	}
	// If we already have a relationship with an ascending node and that has the
	// same root key and sequence number (i.e. nothing has changed in the tree since
	// the path was set up) then we don't need to send another bootstrap message just
	// yet. We'll either wait for the path to be torn down, expire or for the tree to
	// change.
	ann := s._rootAnnouncement()
	if asc := s._ascending; asc != nil && asc.Source.started.Load() {
		if asc.RootPublicKey == ann.RootPublicKey && asc.RootSequence == ann.Sequence {
			return
		}
	}
	// Construct the bootstrap packet. We will include our root key and sequence
	// number in the update so that the remote side can determine if we are both using
	// the same root node when processing the update.
	b := frameBufferPool.Get().(*[types.MaxFrameSize]byte)
	payload := b[:8+ed25519.PublicKeySize+ann.Sequence.Length()]
	defer frameBufferPool.Put(b)
	bootstrap := types.VirtualSnakeBootstrap{
		RootPublicKey: ann.RootPublicKey,
		RootSequence:  ann.Sequence,
	}
	// Generate a random path ID.
	if _, err := rand.Read(bootstrap.PathID[:]); err != nil {
		return
	}
	if _, err := bootstrap.MarshalBinary(payload[:]); err != nil {
		return
	}
	// Construct the frame. We set the destination key to be our own public key. As
	// the bootstrap routing defaults to routing towards higher keys, this should
	// mean that the message gets forwarded up to the next highest key from ours.
	send := getFrame()
	send.Type = types.TypeVirtualSnakeBootstrap
	send.DestinationKey = s.r.public
	send.Source = s._coords()
	send.Payload = append(send.Payload[:0], payload...)
	// Bootstrap messages are routed using SNEK routing with special rules for
	// bootstrap packets.
	if p := s._nextHopsSNEK(send, true); p != nil && p.proto != nil {
		p.proto.push(send)
	}
}

// _nextHopsSNEK locates the best next-hop for a given SNEK-routed frame. The
// bootstrap flag determines whether the frame should be routed using bootstrap
// specific rules — this should only be used for VirtualSnakeBootstrap frames.
func (s *state) _nextHopsSNEK(rx *types.Frame, bootstrap bool) *peer {
	destKey := rx.DestinationKey
	// If the message isn't a bootstrap message and the destination is for our
	// own public key, handle the frame locally — it's basically loopback.
	if !bootstrap && s.r.public == destKey {
		return s.r.local
	}
	rootAnn := s._rootAnnouncement()
	// We start off with our own key as the best key. Any suitable next-hop
	// candidate has to improve on our own key in order to forward the frame,
	// otherwise we'll return the local router port instead.
	bestKey := s.r.public
	bestPeer := s.r.local
	// newCandidate updates the best key and best peer with new candidates.
	newCandidate := func(key types.PublicKey, p *peer) {
		bestKey, bestPeer = key, p
	}
	// newCheckedCandidate performs some sanity checks on the candidate before
	// passing it to newCandidate.
	newCheckedCandidate := func(candidate types.PublicKey, p *peer) {
		switch {
		case !bootstrap && candidate == destKey && bestKey != destKey:
			newCandidate(candidate, p)
		case util.DHTOrdered(destKey, candidate, bestKey):
			newCandidate(candidate, p)
		}
	}

	// Check if we can use the path to the root via our parent as a starting
	// point. We can't do this if we are the root node as there would be no
	// parent or ascending paths.
	if s._parent != nil && s._parent.started.Load() {
		switch {
		case bootstrap && bestKey == destKey:
			// Bootstraps always start working towards the root so that they
			// go somewhere rather than getting stuck.
			fallthrough
		case util.DHTOrdered(bestKey, destKey, rootAnn.RootPublicKey):
			// The destination key is higher than our own key, so start using
			// the path to the root as the first candidate.
			newCandidate(rootAnn.RootPublicKey, s._parent)
		}

		// Check our direct ancestors in the tree, that is, all nodes between
		// ourselves and the root node via the parent port.
		if ann := s._announcements[s._parent]; ann != nil {
			for _, ancestor := range ann.Signatures {
				newCheckedCandidate(ancestor.PublicKey, s._parent)
			}
		}
	}

	// Check all of the ancestors of our direct peers too, that is, all nodes
	// between our direct peer and the root node.
	for p, ann := range s._announcements {
		if !p.started.Load() {
			continue
		}
		for _, hop := range ann.Signatures {
			newCheckedCandidate(hop.PublicKey, p)
		}
	}

	// Check whether our current best candidate is actually a direct peer.
	// This might happen if we spotted the node in our direct ancestors for
	// example, only in this case it would make more sense to route directly
	// to the peer via our peering with them as opposed to routing via our
	// parent port.
	for p := range s._announcements {
		if !p.started.Load() {
			continue
		}
		if peerKey := p.public; bestKey == peerKey {
			// We've seen this key already and we are directly peered, so use
			// the peering instead of the previous selected port.
			newCandidate(peerKey, p)
		}
	}

	// Check our DHT entries. In particular, we are only looking at the source
	// side of the DHT paths. Since setups travel from the lower key to the
	// higher one, this is effectively looking for paths that descend through
	// keyspace toward lower keys rather than ascend toward higher ones.
	for _, entry := range s._table {
		if !entry.Source.started.Load() || !entry.valid() || entry.Source == s.r.local {
			continue
		}
		newCheckedCandidate(entry.PublicKey, entry.Source)
	}

	return bestPeer
}

// _handleBootstrap is called in response to receiving a bootstrap packet.
// This function will send a bootstrap ACK back to the sender.
func (s *state) _handleBootstrap(from *peer, rx *types.Frame) error {
	// Unmarshal the bootstrap.
	var bootstrap types.VirtualSnakeBootstrap
	_, err := bootstrap.UnmarshalBinary(rx.Payload)
	if err != nil {
		return fmt.Errorf("bootstrap.UnmarshalBinary: %w", err)
	}
	// In response to a bootstrap, we'll send back a bootstrap ACK packet to
	// the sender. We'll include our own root details in the ACK.
	root := s._rootAnnouncement()
	bootstrapACK := types.VirtualSnakeBootstrapACK{
		PathID:        bootstrap.PathID,
		RootPublicKey: root.RootPublicKey,
		RootSequence:  root.Sequence,
	}
	b := frameBufferPool.Get().(*[types.MaxFrameSize]byte)
	buf := b[:8+ed25519.PublicKeySize+root.Sequence.Length()]
	defer frameBufferPool.Put(b)
	if _, err := bootstrapACK.MarshalBinary(buf[:]); err != nil {
		return fmt.Errorf("bootstrapACK.MarshalBinary: %w", err)
	}
	// Bootstrap ACKs are routed using tree routing, so we need to take the
	// coordinates from the source field of the received packet and set the
	// destination of the ACK packet to that.
	send := getFrame()
	send.Type = types.TypeVirtualSnakeBootstrapACK
	send.Destination = rx.Source
	send.DestinationKey = rx.DestinationKey
	send.Source = s._coords()
	send.SourceKey = s.r.public
	send.Payload = append(send.Payload[:0], buf...)
	if p := s._nextHopsTree(s.r.local, send); p != nil && p.proto != nil {
		p.proto.push(send)
	}
	return nil
}

// _handleBootstrapACK is called in response to receiving a bootstrap ACK
// packet. This function will work out whether the remote node is a suitable
// candidate to set up an outbound path to, and if so, will send path setup
// packets to the network.
func (s *state) _handleBootstrapACK(from *peer, rx *types.Frame) error {
	// Unmarshal the bootstrap ACK.
	var bootstrapACK types.VirtualSnakeBootstrapACK
	_, err := bootstrapACK.UnmarshalBinary(rx.Payload)
	if err != nil {
		return fmt.Errorf("bootstrapACK.UnmarshalBinary: %w", err)
	}
	root := s._rootAnnouncement()
	update := false
	asc := s._ascending
	switch {
	case rx.SourceKey == s.r.public:
		// We received a bootstrap ACK from ourselves. This shouldn't happen,
		// so either another node has forwarded it to us incorrectly, or
		// a routing loop has occurred somewhere. Don't act on the bootstrap
		// in that case.
	case bootstrapACK.RootPublicKey != root.RootPublicKey:
		// The root key in the bootstrap ACK doesn't match our own key, so
		// routing setup packets using tree routing would fail.
	case bootstrapACK.RootSequence != root.Sequence:
		// The root sequence number in the bootstrap ACK doesn't match our own
		// root sequence, so it seems that they have a different root announcement
		// to us.
	case asc != nil && asc.valid():
		// We already have an ascending entry and it hasn't expired yet.
		switch {
		case asc.PublicKey == rx.SourceKey && bootstrapACK.PathID != asc.PathID:
			// We've received another bootstrap ACK from our direct ascending node.
			// Just refresh the record and then send a new path setup message to
			// that node.
			update = true
		case util.DHTOrdered(s.r.public, rx.SourceKey, asc.Origin):
			// We know about an ascending node already but it turns out that this
			// new node that we've received a bootstrap from is actually closer to
			// us than the previous node. We'll update our record to use the new
			// node instead and then send a new path setup message to it.
			update = true
		}
	case asc == nil || !asc.valid():
		// We don't have an ascending entry, or we did but it expired.
		if util.LessThan(s.r.public, rx.SourceKey) {
			// We don't know about an ascending node and at the moment we don't know
			// any better candidates, so we'll accept a bootstrap ACK from a node with a
			// key higher than ours (so that it matches descending order).
			update = true
		}
	default:
		// The bootstrap ACK conditions weren't met. This might just be because
		// there's a node out there that hasn't converged to a closer node
		// yet, so we'll just ignore the acknowledgement.
	}
	// If we haven't decided we like the update then we won't do anything at this
	// point so give up.
	if !update {
		return nil
	}
	// Include our own root information in the update.
	setup := types.VirtualSnakeSetup{ // nolint:gosimple
		PathID:        bootstrapACK.PathID,
		RootPublicKey: root.RootPublicKey,
		RootSequence:  root.Sequence,
	}
	b := frameBufferPool.Get().(*[types.MaxFrameSize]byte)
	buf := b[:8+ed25519.PublicKeySize+root.Sequence.Length()]
	defer frameBufferPool.Put(b)
	if _, err := setup.MarshalBinary(buf[:]); err != nil {
		return fmt.Errorf("setup.MarshalBinary: %w", err)
	}
	// Setup messages routed using tree routing. The destination key is set in the
	// header so that a node can determine if the setup message arrived at the
	// intended destination instead of forwarding it. The source key is set to our
	// public key, since this is the lower of the two keys that intermediate nodes
	// will populate into their routing tables.
	send := getFrame()
	send.Type = types.TypeVirtualSnakeSetup
	send.Destination = rx.Source
	send.DestinationKey = rx.SourceKey
	send.SourceKey = s.r.public
	send.Payload = append(send.Payload[:0], buf...)
	nexthop := s.r.state._nextHopsTree(s.r.local, send)
	// Importantly, we will only create a DHT entry if it appears as though our next
	// hop has actually accepted the packet. Otherwise we'll create a path entry and
	// the setup message won't go anywhere.
	switch {
	case nexthop == nil:
		fallthrough // No peer was identified, which shouldn't happen.
	case nexthop.local():
		fallthrough // The peer is local, which shouldn't happen.
	case !nexthop.started.Load():
		fallthrough // The peer has shut down or errored.
	case nexthop.proto == nil:
		fallthrough // The peer doesn't have a protocol queue for some reason.
	case !nexthop.proto.push(send):
		return nil // We failed to push the message into the peer queue.
	}
	index := virtualSnakeIndex{
		PublicKey: s.r.public,
		PathID:    bootstrapACK.PathID,
	}
	entry := &virtualSnakeEntry{
		virtualSnakeIndex: &index,
		Origin:            rx.SourceKey,
		Source:            s.r.local,
		Destination:       nexthop,
		LastSeen:          time.Now(),
		RootPublicKey:     bootstrapACK.RootPublicKey,
		RootSequence:      bootstrapACK.RootSequence,
	}
	// The remote side is responsible for clearing up the replaced path, but
	// we do want to make sure we don't have any old paths to other nodes
	// that *aren't* the new ascending node lying around. This helps to avoid
	// routing loops.
	for dhtKey, entry := range s._table {
		if entry.Source == s.r.local && entry.PublicKey != rx.SourceKey {
			s._sendTeardownForExistingPath(s.r.local, dhtKey.PublicKey, dhtKey.PathID)
		}
	}
	// Install the new route into the DHT.
	s._table[index] = entry
	s._ascending = entry
	return nil
}

// _handleSetup is called in response to receiving setup packets. Note that
// these packets are handled even as we forward them, as setup packets should be
// processed by each node on the path.
func (s *state) _handleSetup(from *peer, rx *types.Frame, nexthop *peer) error {
	root := s._rootAnnouncement()
	// Unmarshal the setup.
	var setup types.VirtualSnakeSetup
	if _, err := setup.UnmarshalBinary(rx.Payload); err != nil {
		return fmt.Errorf("setup.UnmarshalBinary: %w", err)
	}
	if setup.RootPublicKey != root.RootPublicKey || setup.RootSequence != root.Sequence {
		s._sendTeardownForRejectedPath(rx.SourceKey, setup.PathID, from)
		return nil
	}
	index := virtualSnakeIndex{
		PublicKey: rx.SourceKey,
		PathID:    setup.PathID,
	}
	// If we already have a path for this public key and path ID combo, which
	// *shouldn't* happen, then we need to tear down both the existing path and
	// then send back a teardown to the sender notifying them that there was a
	// problem. This will probably trigger a new setup, but that's OK, it should
	// have a new path ID.
	if _, ok := s._table[index]; ok {
		s._sendTeardownForExistingPath(s.r.local, rx.SourceKey, setup.PathID) // first call fixes routing table
		s._sendTeardownForRejectedPath(rx.SourceKey, setup.PathID, from)      // second call sends back to origin
		return nil
	}
	// If we're at the destination of the setup then update our predecessor
	// with information from the bootstrap.
	if rx.DestinationKey == s.r.public {
		update := false
		desc := s._descending
		switch {
		case setup.RootPublicKey != root.RootPublicKey:
			// The root key in the setup packet doesn't match our own key, so
			// routing setup packets using tree routing would fail.
		case setup.RootSequence != root.Sequence:
			// The root sequence number in the setup packet doesn't match our own
			// root sequence, so it seems that they have a different root announcement
			// to us.
		case !util.LessThan(rx.SourceKey, s.r.public):
			// The bootstrapping key should be less than ours but it isn't.
		case desc != nil && desc.valid():
			// We already have a descending entry and it hasn't expired.
			switch {
			case desc.PublicKey == rx.SourceKey && setup.PathID != desc.PathID:
				// We've received another bootstrap from our direct descending node.
				// Send back an acknowledgement as this is OK.
				update = true
			case util.DHTOrdered(desc.PublicKey, rx.SourceKey, s.r.public):
				// The bootstrapping node is closer to us than our previous descending
				// node was.
				update = true
			}
		case desc == nil || !desc.valid():
			// We don't have a descending entry, or we did but it expired.
			if util.LessThan(rx.SourceKey, s.r.public) {
				// The bootstrapping key is less than ours so we'll acknowledge it.
				update = true
			}
		default:
			// The bootstrap conditions weren't met. This might just be because
			// there's a node out there that hasn't converged to a closer node
			// yet, so we'll just ignore the bootstrap.
		}
		if !update {
			s._sendTeardownForRejectedPath(rx.SourceKey, setup.PathID, from)
			return nil
		}
		if desc != nil {
			// Tear down the previous path, if there was one.
			s._sendTeardownForExistingPath(s.r.local, desc.PublicKey, desc.PathID)
		}
		entry := &virtualSnakeEntry{
			virtualSnakeIndex: &index,
			Origin:            rx.SourceKey,
			Source:            from,
			Destination:       s.r.local,
			LastSeen:          time.Now(),
			RootPublicKey:     setup.RootPublicKey,
			RootSequence:      setup.RootSequence,
		}
		s._table[index] = entry
		s._descending = entry
		return nil
	}
	// Try to forward the setup onto the next node first. If we
	// can't do that then there's no point in keeping the path.
	switch {
	case nexthop == nil:
		fallthrough // No peer was identified, which shouldn't happen.
	case nexthop.local():
		fallthrough // The peer is local, which shouldn't happen.
	case !nexthop.started.Load():
		fallthrough // The peer has shut down or errored.
	case nexthop.proto == nil:
		fallthrough // The peer doesn't have a protocol queue for some reason.
	case !nexthop.proto.push(rx):
		s._sendTeardownForRejectedPath(rx.SourceKey, setup.PathID, from)
		return nil // We failed to push the message into the peer queue.
	}
	// Add a new routing table entry as we are intermediate to
	// the path.
	s._table[index] = &virtualSnakeEntry{
		virtualSnakeIndex: &index,
		Origin:            rx.SourceKey,
		LastSeen:          time.Now(),
		RootPublicKey:     setup.RootPublicKey,
		RootSequence:      setup.RootSequence,
		Source:            from,    // node with lower of the two keys
		Destination:       nexthop, // node with higher of the two keys
	}
	return nil
}

// _handleTeardown is called in response to receiving a teardown packet from the
// network.
func (s *state) _handleTeardown(from *peer, rx *types.Frame) ([]*peer, error) {
	if len(rx.Payload) < 8 {
		return nil, fmt.Errorf("payload too short")
	}
	var teardown types.VirtualSnakeTeardown
	if _, err := teardown.UnmarshalBinary(rx.Payload); err != nil {
		return nil, fmt.Errorf("teardown.UnmarshalBinary: %w", err)
	}
	return s._teardownPath(from, rx.DestinationKey, teardown.PathID), nil
}

// _sendTeardownForRejectedPath sends a teardown into the network for a path
// that was received but not accepted.
func (s *state) _sendTeardownForRejectedPath(pathKey types.PublicKey, pathID types.VirtualSnakePathID, via *peer) {
	if via != nil {
		via.proto.push(s._getTeardown(pathKey, pathID))
	}
}

// _sendTeardownForExistingPath sends a teardown into the network for a path
// that was already accepted into the routing table but is being replaced or
// removed.
func (s *state) _sendTeardownForExistingPath(from *peer, pathKey types.PublicKey, pathID types.VirtualSnakePathID) {
	frame := s._getTeardown(pathKey, pathID)
	for _, nexthop := range s._teardownPath(from, pathKey, pathID) {
		if nexthop != nil && nexthop.proto != nil {
			nexthop.proto.push(frame)
		}
	}
}

// _getTeardown generates a frame containing a teardown message for the given
// path key and path ID.
func (s *state) _getTeardown(pathKey types.PublicKey, pathID types.VirtualSnakePathID) *types.Frame {
	var payload [8]byte
	teardown := types.VirtualSnakeTeardown{
		PathID: pathID,
	}
	if _, err := teardown.MarshalBinary(payload[:]); err != nil {
		return nil
	}
	frame := getFrame()
	frame.Type = types.TypeVirtualSnakeTeardown
	frame.DestinationKey = pathKey
	frame.Payload = append(frame.Payload[:0], payload[:]...)
	return frame
}

// _teardownPath processes a teardown message by tearing down any
// related routes, returning a slice of next-hop candidates that the
// teardown must be forwarded to.
func (s *state) _teardownPath(from *peer, pathKey types.PublicKey, pathID types.VirtualSnakePathID) []*peer {
	if asc := s._ascending; asc != nil && asc.PublicKey == pathKey && asc.PathID == pathID {
		switch {
		case from.local(): // originated locally
			fallthrough
		case from == asc.Destination: // from network
			s._ascending = nil
			delete(s._table, virtualSnakeIndex{asc.PublicKey, asc.PathID})
			defer s._bootstrapNow()
			return []*peer{asc.Destination}
		}
	}
	if desc := s._descending; desc != nil && desc.PublicKey == pathKey && desc.PathID == pathID {
		switch {
		case from == desc.Source: // from network
			fallthrough
		case from.local(): // originated locally
			s._descending = nil
			delete(s._table, virtualSnakeIndex{desc.PublicKey, desc.PathID})
			return []*peer{desc.Source}
		}
	}
	for k, v := range s._table {
		if k.PublicKey == pathKey && k.PathID == pathID {
			switch {
			case from.local(): // happens when we're tearing down an existing duplicate path
				delete(s._table, k)
				return []*peer{v.Destination, v.Source}
			case from == v.Source: // from network, return the opposite direction
				delete(s._table, k)
				return []*peer{v.Destination}
			case from == v.Destination: // from network, return the opposite direction
				delete(s._table, k)
				return []*peer{v.Source}
			}
		}
	}
	return nil
}
