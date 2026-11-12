package peer

import (
	"context"
	"sync"

	"github.com/lightningnetwork/lnd/lnwire"
)

// rbfFeeBumpSlots lets one fee bump per channel at a time wait for and send
// its offer. The zero value is ready to use.
//
// A slot is a channel with room for one token: holding the token is holding
// the slot. A bump that waits for the slot can still be cancelled, which a
// mutex would not allow.
type rbfFeeBumpSlots struct {
	mu    sync.Mutex
	slots map[lnwire.ChannelID]chan struct{}
}

// acquire waits until the channel's slot is free and takes it, or returns the
// context's error if the context ends first. The returned function frees the
// slot and must be called once.
func (s *rbfFeeBumpSlots) acquire(ctx context.Context,
	chanID lnwire.ChannelID) (func(), error) {

	s.mu.Lock()
	if s.slots == nil {
		s.slots = make(map[lnwire.ChannelID]chan struct{})
	}
	slot, ok := s.slots[chanID]
	if !ok {
		slot = make(chan struct{}, 1)
		s.slots[chanID] = slot
	}
	s.mu.Unlock()

	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil

	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
