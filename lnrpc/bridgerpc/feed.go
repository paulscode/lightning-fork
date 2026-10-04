//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/paulscode/lightning-fork-bridge/rate"
)

// priceSource is where the service gets the rate it trades at: the operator's
// own (ratebook) or the market's (feed).
type priceSource interface {
	// quoteRate is the rate to trade at now, or why there is none.
	quoteRate() (marketRate, error)

	// last is the most recent rate known, usable or not, and when it was
	// set or measured: what Status shows, and what the toBLAKE2b bounds
	// are converted at. Zero when there has never been one.
	last() (float64, time.Time)
}

// marketRate is a rate to trade at.
type marketRate struct {
	// rate is SHA256 coin per BLAKE2b coin.
	rate float64

	// at is when it was set or measured.
	at time.Time

	// volatility is how far the market itself moved recently, as a
	// fraction, which the fee is widened by. Zero for a fixed rate, which
	// has no market to measure.
	volatility float64

	// sources is how many readings agreed on it: one for the operator's
	// own, two for the market cross-checked.
	sources int
}

// quoteRate is the operator's rate while it is fresh.
func (b *ratebook) quoteRate() (marketRate, error) {
	r, at, err := b.usable()
	if err != nil {
		return marketRate{}, err
	}

	return marketRate{rate: r, at: at, sources: 1}, nil
}

// last is the operator's rate, fresh or not.
func (b *ratebook) last() (float64, time.Time) {
	return b.current()
}

// The market the feed follows.
//
// Neoxa is where BTCB2 trades, and its BTCB2_BTC book is the price payers'
// own wallets check a service's quote against (the dashboard's reference
// rate), so a bridge priced from it charges payers exactly its fee over what
// they see. Its BTCB2_USDC book, over a BTC/USD price, is the cross-check: a
// different book that should say the same thing, and when it does not, one of
// them is wrong and there is no telling which.
const (
	neoxaTickerURL = "https://neoxa.exchange/api/exchange/ticker/"

	// The pair priced from, and the one cross-checked against.
	pairDirect = "BTCB2_BTC"
	pairUSDC   = "BTCB2_USDC"

	// BTC/USD for the cross-check: a mainstream price first, and
	// Neoxa's own BTC_USDC book when that cannot be read (CoinGecko
	// refuses many Tor exits). Either serves: the check is for a gap of
	// several percent, and both are well inside that of each other.
	coinGeckoRatesURL = "https://api.coingecko.com/api/v3/exchange_rates"
	pairBTCUSDC       = "BTC_USDC"

	// feedUserAgent names the bridge to the sources.
	feedUserAgent = "lightning-fork-bridge"
)

// Timing.
const (
	// feedInterval is how often the market is read.
	feedInterval = 30 * time.Second

	// feedTimeout bounds one reading of every source.
	feedTimeout = 20 * time.Second

	// smoothWindow is how far back readings are taken into the median
	// the bridge trades at: the last three or four. A median, not an
	// average: one odd reading (a book emptied for a moment on one side)
	// moves it not at all, and it lags the market by a reading at most,
	// where a longer average would hand anyone watching a stale price to
	// trade against.
	smoothWindow = 100 * time.Second

	// tickerFrozenAfter is how old Neoxa's own computedAt may be. It
	// recomputes its tickers every few seconds; one minutes old is a
	// ticker that has stopped, whatever the request says. Generous,
	// because it compares Neoxa's clock with this node's.
	tickerFrozenAfter = 5 * time.Minute

	// maxFeedBody bounds a response.
	maxFeedBody = 1 << 20
)

var (
	// errNoReading is the feed before its first reading.
	errNoReading = errors.New("the market has not been read yet")

	// errFeedUnreadable is a source that could not be read.
	errFeedUnreadable = errors.New("the market could not be read")
)

// fetchFunc reads a URL.
type fetchFunc func(ctx context.Context, url string) ([]byte, error)

// feed follows the market.
type feed struct {
	fetch fetchFunc
	now   func() time.Time

	policy rate.Policy
	oracle *rate.Oracle

	// onReading is called after every reading that produced a rate, for
	// the server to bring up a direction held for want of one. Outside
	// the lock.
	onReading func()

	mu sync.Mutex

	// readings are the recent good ones, oldest first.
	readings []rate.Reading

	// cross is the cross-check's last value, for Status.
	cross float64

	// err is why the last reading produced no rate, nil when it did.
	// unsafe says it was a refusal on the numbers (the books disagree,
	// the breaker tripped, a ticker stopped) rather than a source that
	// did not answer: that stops quoting at once, where an unanswered
	// reading leaves the last good one usable until it is too old.
	err    error
	unsafe bool
}

// newFeed builds a feed reading through dial: the node's own dialler, so that
// a node set to reach the world through Tor reads the market through Tor too.
// Nil dials directly.
func newFeed(policy rate.Policy,
	dial func(network, address string, timeout time.Duration) (net.Conn,
		error)) (*feed, error) {

	oracle, err := rate.New(policy)
	if err != nil {
		return nil, err
	}

	return &feed{
		fetch: marketFetch(dial), now: time.Now, policy: policy,
		oracle: oracle,
	}, nil
}

// marketFetch builds what the feed reads with: httpFetch, except in tests,
// which must not reach the network.
var marketFetch = httpFetch

// httpFetch reads URLs over dial.
func httpFetch(dial func(network, address string,
	timeout time.Duration) (net.Conn, error)) fetchFunc {

	transport := &http.Transport{
		TLSHandshakeTimeout: feedTimeout,
		MaxIdleConns:        4,
		IdleConnTimeout:     2 * feedInterval,
	}
	if dial != nil {
		transport.DialContext = func(ctx context.Context, network,
			address string) (net.Conn, error) {

			timeout := feedTimeout
			if d, ok := ctx.Deadline(); ok {
				timeout = time.Until(d)
			}

			return dial(network, address, timeout)
		}
	}
	client := &http.Client{Transport: transport, Timeout: feedTimeout}

	return func(ctx context.Context, url string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url,
			nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", feedUserAgent)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}

		return io.ReadAll(io.LimitReader(resp.Body, maxFeedBody))
	}
}

// run reads the market until ctx ends.
func (f *feed) run(ctx context.Context) {
	tick := time.NewTicker(feedInterval)
	defer tick.Stop()

	for {
		f.read(ctx)

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// read takes one reading of the market.
func (f *feed) read(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, feedTimeout)
	defer cancel()

	r, cross, err, unsafe := f.measure(ctx)

	f.mu.Lock()
	wasErr := f.err
	f.err, f.unsafe = err, unsafe
	if cross > 0 {
		f.cross = cross
	}
	if err == nil {
		f.readings = append(f.readings, r)
		// Twice the window is all the median and Status need.
		cut := 0
		for cut < len(f.readings)-1 &&
			r.At.Sub(f.readings[cut].At) > 2*smoothWindow {

			cut++
		}
		f.readings = f.readings[cut:]
	}
	f.mu.Unlock()

	switch {
	case err != nil && (wasErr == nil ||
		wasErr.Error() != err.Error()):

		log.Warnf("Bridge market rate: %v", err)

	case err == nil && wasErr != nil:
		log.Infof("Bridge market rate: reading again, %g SHA256 coin "+
			"per BLAKE2b coin", r.OutgoingPerIncoming)
	}

	if err == nil && f.onReading != nil {
		f.onReading()
	}
}

// measure reads every source and decides.
func (f *feed) measure(ctx context.Context) (rate.Reading, float64, error,
	bool) {

	now := f.now()

	direct, err := f.ticker(ctx, pairDirect, now)
	if err != nil {
		return rate.Reading{}, 0, err, errors.Is(err, rate.ErrStale)
	}
	usdc, err := f.ticker(ctx, pairUSDC, now)
	if err != nil {
		return rate.Reading{}, 0, fmt.Errorf("the cross-check: %w", err),
			errors.Is(err, rate.ErrStale)
	}
	btcUSD, err := f.btcUSD(ctx, now)
	if err != nil {
		return rate.Reading{}, 0, fmt.Errorf("the cross-check: %w", err),
			errors.Is(err, rate.ErrStale)
	}

	cross := usdc / btcUSD
	if gap := math.Abs(direct-cross) / math.Min(direct, cross); gap >
		f.policy.MaxDisagreement {

		return rate.Reading{}, cross, fmt.Errorf("%w: Neoxa's %s market "+
			"says %.8g; its %s market over BTC/USD says %.8g, %.1f%% "+
			"apart, more than the %.1f%% allowed",
			rate.ErrDisagreement, pairDirect, direct, pairUSDC, cross,
			gap*100, f.policy.MaxDisagreement*100), true
	}

	// Priced from the direct book alone: it is the one payers check
	// against, and the oracle's median of two would take the higher, which
	// for toSHA256 is the payer's side of a gap up to the disagreement
	// limit. The cross-check has done its job by agreeing.
	r, err := f.oracle.Update(now, []rate.Observation{{
		Source: "neoxa " + pairDirect, OutgoingPerIncoming: direct,
		At: now,
	}})
	if err != nil {
		return rate.Reading{}, cross, err, true
	}

	return r, cross, nil, false
}

// neoxaTicker is the part of Neoxa's ticker the feed reads.
type neoxaTicker struct {
	Success bool   `json:"success"`
	Pair    string `json:"pair"`
	Ticker  struct {
		LastPrice  float64 `json:"lastPrice"`
		BestBid    float64 `json:"bestBid"`
		BestAsk    float64 `json:"bestAsk"`
		ComputedAt int64   `json:"computedAt"`
	} `json:"ticker"`
}

// ticker reads one of Neoxa's books: the middle of its last trade, best bid
// and best ask.
//
// The middle one, as the dashboard reads it for payers. On a book this thin a
// single trade, made by anyone at any price the book allows, would otherwise
// be the price; while orders stand on both sides it moves nothing. Without a
// usable book, the last trade alone.
func (f *feed) ticker(ctx context.Context, pair string, now time.Time) (float64,
	error) {

	raw, err := f.fetch(ctx, neoxaTickerURL+pair)
	if err != nil {
		return 0, fmt.Errorf("%w: Neoxa's %s market: %v",
			errFeedUnreadable, pair, err)
	}

	var t neoxaTicker
	if err := json.Unmarshal(raw, &t); err != nil || !t.Success ||
		(t.Pair != "" && t.Pair != pair) {

		return 0, fmt.Errorf("%w: Neoxa's %s market answered with "+
			"something other than its ticker", errFeedUnreadable, pair)
	}

	if t.Ticker.ComputedAt > 0 {
		computed := time.UnixMilli(t.Ticker.ComputedAt)
		if age := now.Sub(computed); age > tickerFrozenAfter {
			return 0, fmt.Errorf("%w: Neoxa's %s ticker has not "+
				"changed since %s", rate.ErrStale, pair,
				computed.UTC().Format(time.RFC3339))
		}
	}

	last, bid, ask := t.Ticker.LastPrice, t.Ticker.BestBid, t.Ticker.BestAsk
	if !positive(last) {
		return 0, fmt.Errorf("%w: Neoxa's %s market has no last price",
			errFeedUnreadable, pair)
	}
	if !positive(bid) || !positive(ask) || bid > ask {
		return last, nil
	}
	prices := []float64{last, bid, ask}
	sort.Float64s(prices)

	return prices[1], nil
}

// btcUSD is dollars per BTC: CoinGecko's, or Neoxa's own book.
func (f *feed) btcUSD(ctx context.Context, now time.Time) (float64, error) {
	raw, err := f.fetch(ctx, coinGeckoRatesURL)
	if err == nil {
		var cg struct {
			Rates map[string]struct {
				Value float64 `json:"value"`
			} `json:"rates"`
		}
		if json.Unmarshal(raw, &cg) == nil {
			// The rates are per BTC, so the dollar's is dollars
			// per BTC.
			if usd := cg.Rates["usd"].Value; positive(usd) {
				return usd, nil
			}
		}
	}

	return f.ticker(ctx, pairBTCUSDC, now)
}

func positive(v float64) bool {
	return v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v)
}

// quoteRate is the median of the recent readings, while the newest is fresh.
func (f *feed) quoteRate() (marketRate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil && f.unsafe {
		return marketRate{}, f.err
	}
	if len(f.readings) == 0 {
		if f.err != nil {
			return marketRate{}, f.err
		}

		return marketRate{}, errNoReading
	}

	newest := f.readings[len(f.readings)-1]
	if age := f.now().Sub(newest.At); age > f.policy.MaxAge {
		why := ""
		if f.err != nil {
			why = fmt.Sprintf(" (%v)", f.err)
		}

		return marketRate{}, fmt.Errorf("%w: the market was last read "+
			"%v ago, and the bridge trades only on a reading at most "+
			"%v old%s", rate.ErrStale, age.Round(time.Second),
			f.policy.MaxAge, why)
	}

	var recent []float64
	for _, r := range f.readings {
		if newest.At.Sub(r.At) <= smoothWindow {
			recent = append(recent, r.OutgoingPerIncoming)
		}
	}
	sort.Float64s(recent)
	mid := recent[len(recent)/2]
	if len(recent)%2 == 0 {
		mid = (recent[len(recent)/2-1] + mid) / 2
	}

	return marketRate{
		rate: mid, at: newest.At, volatility: newest.Volatility,
		sources: 2,
	}, nil
}

// last is the newest reading, fresh or not.
func (f *feed) last() (float64, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.readings) == 0 {
		return 0, time.Time{}
	}
	r := f.readings[len(f.readings)-1]

	return r.OutgoingPerIncoming, r.At
}

// crossCheck is the cross-check's last value, for Status.
func (f *feed) crossCheck() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.cross
}
