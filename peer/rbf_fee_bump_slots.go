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
// mutex would not allow. A channel's slot is kept only while a bump holds it
// or waits for it.
type rbfFeeBumpSlots struct {
	mu    sync.Mutex
	slots map[lnwire.ChannelID]*rbfFeeBumpSlot
}

// rbfFeeBumpSlot is the slot of one channel.
type rbfFeeBumpSlot struct {
	// token holds one value while a bump holds the slot.
	token chan struct{}

	// users counts the bumps that hold the slot or wait for it.
	users int
}

// acquire waits until the channel's slot is free and takes it, or returns the
// context's error if the context ends first. The returned function frees the
// slot and must be called once.
func (s *rbfFeeBumpSlots) acquire(ctx context.Context,
	chanID lnwire.ChannelID) (func(), error) {

	s.mu.Lock()
	if s.slots == nil {
		s.slots = make(map[lnwire.ChannelID]*rbfFeeBumpSlot)
	}
	slot, ok := s.slots[chanID]
	if !ok {
		slot = &rbfFeeBumpSlot{token: make(chan struct{}, 1)}
		s.slots[chanID] = slot
	}
	slot.users++
	s.mu.Unlock()

	// leave drops the channel's slot once no bump uses it.
	leave := func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		slot.users--
		if slot.users == 0 {
			delete(s.slots, chanID)
		}
	}

	select {
	case slot.token <- struct{}{}:
		return func() {
			<-slot.token
			leave()
		}, nil

	case <-ctx.Done():
		leave()

		return nil, ctx.Err()
	}
}
