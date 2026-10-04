//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/paulscode/lightning-fork-bridge/rate"
)

// rateFileName is where the rate in force is kept, beside the journal.
const rateFileName = "rate.json"

// ratebook is the rate the bridge trades at, who set it and when.
//
// The operator's rate is the price on a market this thin, and the market
// moves: the BLAKE2b coin has gone half again against Bitcoin in under three
// weeks. So the rate can be changed while the bridge runs, it survives a
// restart, and it carries the moment it was set, so that one nobody has looked
// at for too long stops being used rather than quietly pricing swaps.
type ratebook struct {
	path string

	// maxAge is how long a rate is used after it was set. Zero means
	// forever.
	maxAge time.Duration

	now func() time.Time

	mu    sync.RWMutex
	rate  float64
	setAt time.Time
}

// rateFile is the rate on disk.
//
// Configured is the rate the configuration named when this one was set, so a
// rate set at runtime survives a restart, but a configuration edited since
// wins: that edit is the newer decision.
//
// Source says which of the two the rate itself is: "configured" for the
// configuration's own, "runtime" for one set while running. Files written
// before it existed are told apart by comparing Rate with Configured.
type rateFile struct {
	Rate       float64   `json:"rate"`
	SetAt      time.Time `json:"set_at"`
	Configured float64   `json:"configured"`
	Source     string    `json:"source,omitempty"`
}

// Where a rate came from.
const (
	rateFromConfig  = "configured"
	rateFromRuntime = "runtime"
)

// setAtRuntime reports whether a file's rate was set while running rather
// than copied from the configuration.
func (f rateFile) setAtRuntime() bool {
	if f.Source != "" {
		return f.Source == rateFromRuntime
	}

	return f.Rate != f.Configured
}

// openRatebook loads the rate in force: the one last set at runtime if the
// configuration has not changed since, otherwise the configuration's own.
func openRatebook(dir string, configured float64, maxAge time.Duration,
	now func() time.Time) (*ratebook, error) {

	if now == nil {
		now = time.Now
	}
	b := &ratebook{
		path: filepath.Join(dir, rateFileName), maxAge: maxAge,
		now: now, rate: configured, setAt: now(),
	}

	// No configured rate means the operator has not chosen one yet, which
	// is a state the bridge starts in rather than one that stops the node:
	// someone turning the bridge on from a UI sets the rate afterwards, on
	// the same screen that shows them the market. Until then nothing is
	// quoted (usable says why), and nothing is written, because a file
	// holding no rate is one the next start would rightly refuse.
	if configured == 0 {
		b.setAt = time.Time{}
	}

	raw, err := os.ReadFile(b.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if configured == 0 {
			return b, nil
		}

		return b, b.save(configured, rateFromConfig)

	case err != nil:
		return nil, fmt.Errorf("reading the bridge's rate: %w", err)
	}

	var f rateFile
	if err := json.Unmarshal(raw, &f); err != nil {
		// A rate file that cannot be read is not a reason to trade at
		// a guess, and it is not a reason to fall back to the
		// configuration silently either: the operator set something,
		// and what it was is unknown.
		return nil, fmt.Errorf("the bridge's rate file %s cannot be "+
			"read (%v); set the rate again or remove the file to use "+
			"the configured one", b.path, err)
	}
	if !usableRate(f.Rate) || f.SetAt.IsZero() {
		return nil, fmt.Errorf("the bridge's rate file %s holds no "+
			"usable rate; set the rate again or remove the file",
			b.path)
	}

	if f.Configured == configured {
		b.rate, b.setAt = f.Rate, f.SetAt

		return b, nil
	}

	if configured == 0 {
		// A rate set at runtime is kept when the configuration stops
		// naming one: that edit was not about the rate the operator
		// set since. A rate that was only ever the configuration's is
		// not: removing it from the configuration is removing it, and
		// trading on at it would be the bridge choosing a price.
		if f.setAtRuntime() {
			b.rate, b.setAt = f.Rate, f.SetAt
		}

		return b, nil
	}

	// The configuration changed since the rate was last set. That edit is
	// the newer decision, and it is stamped now.
	return b, b.save(configured, rateFromConfig)
}

// usableRate reports whether a number can be traded at.
func usableRate(r float64) bool {
	return r > 0 && !math.IsNaN(r) && !math.IsInf(r, 0)
}

// save records a rate as set now. The caller holds no lock.
func (b *ratebook) save(configured float64, source string) error {
	b.mu.RLock()
	f := rateFile{
		Rate: b.rate, SetAt: b.setAt, Configured: configured,
		Source: source,
	}
	b.mu.RUnlock()

	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}

	// Written whole and renamed, so a crash leaves the old rate or the new
	// one and never half of a file.
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return fmt.Errorf("recording the bridge's rate: %w", err)
	}
	if err := os.Rename(tmp, b.path); err != nil {
		return fmt.Errorf("recording the bridge's rate: %w", err)
	}

	return nil
}

// set changes the rate and records it.
func (b *ratebook) set(r, configured float64) (float64, time.Time, error) {
	if !usableRate(r) {
		return 0, time.Time{}, fmt.Errorf("%w: a rate must be a "+
			"positive number, got %g", ErrConfig, r)
	}

	b.mu.Lock()
	prevRate, prevAt := b.rate, b.setAt
	b.rate, b.setAt = r, b.now()
	at := b.setAt
	b.mu.Unlock()

	if err := b.save(configured, rateFromRuntime); err != nil {
		// Not in force if it is not on disk: a restart would bring the
		// old one back without a word.
		b.mu.Lock()
		b.rate, b.setAt = prevRate, prevAt
		b.mu.Unlock()

		return 0, time.Time{}, err
	}

	return r, at, nil
}

// current is the rate in force and when it was set, whatever its age.
func (b *ratebook) current() (float64, time.Time) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.rate, b.setAt
}

// expiresAt is when the rate stops being used, or the zero time if it does
// not.
func (b *ratebook) expiresAt() time.Time {
	if b.maxAge <= 0 {
		return time.Time{}
	}
	_, at := b.current()
	if at.IsZero() {
		return time.Time{}
	}

	return at.Add(b.maxAge)
}

// ErrNoRate is why nothing is quoted before the operator has set a rate. It
// is a kind of rate.ErrStale, so a payer is told the same thing as for a rate
// that has expired: nothing current to quote on.
var ErrNoRate = fmt.Errorf("%w: no rate has been set yet; set the rate you "+
	"will trade at, as SHA256 coin per BLAKE2b coin (lncli bridge "+
	"setrate, or the Bridge page). There is no default, because a wrong "+
	"one loses money on every swap", rate.ErrStale)

// usable is the rate to quote at, or rate.ErrStale once it is older than the
// limit.
func (b *ratebook) usable() (float64, time.Time, error) {
	r, at := b.current()
	if !usableRate(r) {
		return 0, at, ErrNoRate
	}
	if b.maxAge > 0 {
		if age := b.now().Sub(at); age > b.maxAge {
			return 0, at, fmt.Errorf("%w: the rate was set %v ago and "+
				"the bridge allows %v; set it again",
				rate.ErrStale, age.Round(time.Minute), b.maxAge)
		}
	}

	return r, at, nil
}
