//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-bridge/driver"
	"github.com/paulscode/lightning-fork-bridge/rate"
)

// TestMain keeps every feed in the package's tests off the network: a server
// started with the bridge on follows the market, and the tests that need a
// market give the feed one of their own (testFeed).
func TestMain(m *testing.M) {
	marketFetch = func(func(string, string, time.Duration) (net.Conn,
		error)) fetchFunc {

		return func(context.Context, string) ([]byte, error) {
			return nil, errors.New("tests do not reach the network")
		}
	}
	os.Exit(m.Run())
}

// market is a fake set of sources: each URL answers with what it holds, or
// fails when it holds nothing.
type market struct {
	mu   sync.Mutex
	body map[string]string
	now  time.Time
}

func newMarket(now time.Time) *market {
	m := &market{body: map[string]string{}, now: now}
	m.set(pairDirect, 0.006, 0.006, 0.00606)
	m.set(pairUSDC, 510, 509, 512)
	m.coinGecko(85000)

	return m
}

// set gives a pair a last trade, best bid and best ask.
func (m *market) set(pair string, last, bid, ask float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.body[neoxaTickerURL+pair] = fmt.Sprintf(`{"success":true,`+
		`"pair":%q,"ticker":{"lastPrice":%g,"bestBid":%g,"bestAsk":%g,`+
		`"computedAt":%d}}`, pair, last, bid, ask, m.now.UnixMilli())
}

func (m *market) coinGecko(usd float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if usd == 0 {
		delete(m.body, coinGeckoRatesURL)

		return
	}
	m.body[coinGeckoRatesURL] = fmt.Sprintf(`{"rates":{"btc":{"value":1},`+
		`"usd":{"value":%g}}}`, usd)
}

func (m *market) fail(url string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.body, url)
}

func (m *market) fetch(_ context.Context, url string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.body[url]
	if !ok {
		return nil, errors.New("connection refused")
	}

	return []byte(b), nil
}

// testFeed is a feed over m whose clock is m's.
func testFeed(t *testing.T, m *market) *feed {
	t.Helper()

	c := usable()
	f, err := newFeed(c.resolve().rate, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.fetch = m.fetch
	f.now = func() time.Time {
		m.mu.Lock()
		defer m.mu.Unlock()

		return m.now
	}

	return f
}

// advance moves the clock, and the tickers' own time with it.
func (m *market) advance(d time.Duration) {
	m.mu.Lock()
	m.now = m.now.Add(d)
	now := m.now
	for url, b := range m.body {
		if strings.Contains(b, `"computedAt"`) {
			i := strings.Index(b, `"computedAt":`)
			j := strings.Index(b[i:], "}")
			m.body[url] = b[:i] + fmt.Sprintf(`"computedAt":%d`,
				now.UnixMilli()) + b[i+j:]
		}
	}
	m.mu.Unlock()
}

var feedStart = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// The rate is the direct book's middle price, and the cross-check agreeing
// lets it through.
func TestTheFeedPricesFromTheDirectBook(t *testing.T) {
	m := newMarket(feedStart)
	f := testFeed(t, m)

	if _, err := f.quoteRate(); !errors.Is(err, errNoReading) {
		t.Fatalf("before any reading: %v", err)
	}

	f.read(context.Background())
	got, err := f.quoteRate()
	if err != nil {
		t.Fatal(err)
	}
	// Last 0.006, bid 0.006, ask 0.00606: the middle is 0.006.
	if got.rate != 0.006 || got.sources != 2 {
		t.Errorf("rate %g from %d sources", got.rate, got.sources)
	}
	if cross := f.crossCheck(); math.Abs(cross-510.0/85000) > 1e-12 {
		t.Errorf("cross-check %g", cross)
	}

	// One trade far off the book moves it no further than the book: the
	// middle of 0.009, 0.006 and 0.00606 is the ask.
	m.set(pairDirect, 0.009, 0.006, 0.00606)
	m.advance(30 * time.Second)
	f.read(context.Background())
	if last, _ := f.last(); last != 0.00606 {
		t.Errorf("an off-book trade read as %g", last)
	}
	if got, _ := f.quoteRate(); got.rate > 0.00606 {
		t.Errorf("an off-book trade moved the rate to %g", got.rate)
	}
}

// A book with no usable bid and ask leaves the last trade.
func TestTheFeedFallsBackToTheLastTrade(t *testing.T) {
	m := newMarket(feedStart)
	m.set(pairDirect, 0.006, 0, 0.0061)
	f := testFeed(t, m)

	f.read(context.Background())
	if got, err := f.quoteRate(); err != nil || got.rate != 0.006 {
		t.Errorf("rate %g, %v", got.rate, err)
	}
}

// The median of the last few readings: one odd reading does not move it.
func TestTheFeedSmoothsWithAMedian(t *testing.T) {
	m := newMarket(feedStart)
	f := testFeed(t, m)

	for _, p := range []float64{0.006, 0.00602, 0.0063, 0.00601} {
		m.set(pairDirect, p, p, p)
		// The cross-check follows, so the odd one is not refused for
		// disagreeing: this is about what the median does with it.
		m.set(pairUSDC, p*85000, p*85000, p*85000)
		f.read(context.Background())
		m.advance(30 * time.Second)
	}
	m.advance(-30 * time.Second)

	got, err := f.quoteRate()
	if err != nil {
		t.Fatal(err)
	}
	// Within 100 s of the newest: 0.00602, 0.0063, 0.00601 (the first is
	// 90 s before the newest, so it is in too): median of four is the mean
	// of the middle two, 0.006015.
	if math.Abs(got.rate-0.006015) > 1e-9 {
		t.Errorf("rate %g, wanted the median 0.006015", got.rate)
	}
}

// Two books that disagree stop quoting at once, whatever came before.
func TestTheFeedRefusesWhenTheBooksDisagree(t *testing.T) {
	m := newMarket(feedStart)
	f := testFeed(t, m)
	f.read(context.Background())

	// BTCB2_USDC over BTC/USD says 0.0066, 10% over the direct book.
	m.set(pairUSDC, 561, 561, 561)
	m.advance(30 * time.Second)
	f.read(context.Background())

	_, err := f.quoteRate()
	if !errors.Is(err, rate.ErrDisagreement) {
		t.Fatalf("want a disagreement, got %v", err)
	}
	if !strings.Contains(rateRefusal(err), "disagree") {
		t.Errorf("Status says %q", rateRefusal(err))
	}
}

// A market that cannot be read leaves the last reading usable only while it is
// fresh; after that nothing is quoted.
func TestTheFeedStopsWhenTheMarketCannotBeRead(t *testing.T) {
	m := newMarket(feedStart)
	f := testFeed(t, m)
	f.read(context.Background())

	m.fail(neoxaTickerURL + pairDirect)
	m.advance(30 * time.Second)
	f.read(context.Background())
	if _, err := f.quoteRate(); err != nil {
		t.Fatalf("a reading 30 s old should still serve: %v", err)
	}

	m.advance(70 * time.Second)
	f.read(context.Background())
	_, err := f.quoteRate()
	if !errors.Is(err, rate.ErrStale) {
		t.Fatalf("want stale after 100 s, got %v", err)
	}
	if !strings.Contains(err.Error(), "could not be read") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// Without the cross-check the bridge does not quote either, once the last
// checked reading is too old.
func TestTheCrossCheckIsRequired(t *testing.T) {
	m := newMarket(feedStart)
	m.fail(neoxaTickerURL + pairUSDC)
	f := testFeed(t, m)

	f.read(context.Background())
	if _, err := f.quoteRate(); err == nil ||
		!strings.Contains(err.Error(), "cross-check") {

		t.Fatalf("quoted without a cross-check: %v", err)
	}
}

// CoinGecko not answering (it refuses many Tor exits) falls back to Neoxa's own
// BTC_USDC book for the cross-check.
func TestBTCUSDFallsBackToNeoxa(t *testing.T) {
	m := newMarket(feedStart)
	m.coinGecko(0)
	m.set(pairBTCUSDC, 85000, 84500, 86000)
	f := testFeed(t, m)

	f.read(context.Background())
	if _, err := f.quoteRate(); err != nil {
		t.Fatalf("no rate with CoinGecko down: %v", err)
	}
	if cross := f.crossCheck(); math.Abs(cross-510.0/85000) > 1e-12 {
		t.Errorf("cross-check %g", cross)
	}
}

// A ticker whose own computedAt has stopped is a market that has stopped, and
// it refuses at once.
func TestAFrozenTickerRefuses(t *testing.T) {
	m := newMarket(feedStart)
	f := testFeed(t, m)
	f.read(context.Background())

	m.mu.Lock()
	m.now = m.now.Add(10 * time.Minute)
	m.mu.Unlock()
	f.read(context.Background())

	_, err := f.quoteRate()
	if !errors.Is(err, rate.ErrStale) ||
		!strings.Contains(err.Error(), "has not changed since") {

		t.Fatalf("want a frozen ticker refused, got %v", err)
	}
}

// A move past the breaker stops quoting for the cooldown.
func TestTheBreakerStopsQuoting(t *testing.T) {
	m := newMarket(feedStart)
	f := testFeed(t, m)
	f.read(context.Background())

	// 25% in a minute, both books together.
	m.set(pairDirect, 0.0075, 0.0075, 0.0075)
	m.set(pairUSDC, 637.5, 637.5, 637.5)
	m.advance(time.Minute)
	f.read(context.Background())

	if _, err := f.quoteRate(); !errors.Is(err, rate.ErrBroken) {
		t.Fatalf("want the breaker, got %v", err)
	}

	// Still tripped ten minutes on, though the market is calm.
	m.advance(10 * time.Minute)
	f.read(context.Background())
	if _, err := f.quoteRate(); !errors.Is(err, rate.ErrBroken) {
		t.Fatalf("the breaker cleared inside its cooldown: %v", err)
	}
}

// The volatility the oracle measured is what widens the fee.
func TestTheFeedReportsTheMarketsMovement(t *testing.T) {
	m := newMarket(feedStart)
	f := testFeed(t, m)
	f.read(context.Background())

	m.set(pairDirect, 0.0063, 0.0063, 0.0063)
	m.set(pairUSDC, 535.5, 535.5, 535.5)
	m.advance(30 * time.Second)
	f.read(context.Background())

	got, err := f.quoteRate()
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.volatility-0.05) > 1e-9 {
		t.Errorf("volatility %g, wanted 0.05", got.volatility)
	}
}

// The feed's readings over the network carry its own User-Agent: Neoxa refuses
// some default ones.
func TestTheFeedNamesItself(t *testing.T) {
	if feedUserAgent == "" || strings.Contains(feedUserAgent, "Python") {
		t.Errorf("user agent %q", feedUserAgent)
	}
}

// sidesOf is a service's two directions.
func sidesOf(t *testing.T, svc *service) (forward, reverse *side) {
	t.Helper()
	for _, sd := range svc.sides {
		if sd.invert {
			reverse = sd
		} else {
			forward = sd
		}
	}
	if forward == nil || reverse == nil {
		t.Fatal("want both directions")
	}

	return forward, reverse
}

// Following the market, each direction charges its own fee, widened by how far
// the market has just moved.
func TestTheMarketRateIsQuotedWithEachDirectionsFee(t *testing.T) {
	t.Parallel()

	cfg := usable()
	cfg.RateSource = RateSourceNeoxa
	cfg.FixedRate = 0
	cfg.Spread = 0
	cfg.FeeToSHA256 = 0.02
	cfg.FeeToBLAKE2b = 0.01

	m := newMarket(feedStart)
	mkt := testFeed(t, m)
	mkt.read(context.Background())
	// The market moved 5% within the window.
	m.set(pairDirect, 0.0063, 0.0063, 0.0063)
	m.set(pairUSDC, 535.5, 535.5, 535.5)
	m.advance(30 * time.Second)
	mkt.read(context.Background())

	f := &fakeNode{synced: true,
		balance: cfg.resolve().inventory.TargetOutgoingMsat}
	svc := serviceForMarket(t, cfg, f, mkt)
	svc.positionOf = f.deps().ChannelBalance
	fwd, rev := sidesOf(t, svc)

	// The median of 0.006 and 0.0063.
	want := 0.00615
	got, err := svc.pricer(fwd)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.OutgoingPerIncoming-want) > 1e-12 {
		t.Errorf("forward rate %g, wanted %g", got.OutgoingPerIncoming,
			want)
	}
	if math.Abs(got.Spread-0.07) > 1e-9 {
		t.Errorf("forward spread %g, wanted the 2%% fee plus the 5%% "+
			"move", got.Spread)
	}

	back, err := svc.pricer(rev)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(back.OutgoingPerIncoming-1/want) > 1e-9 {
		t.Errorf("reverse rate %g", back.OutgoingPerIncoming)
	}
	if math.Abs(back.Spread-0.06) > 1e-9 {
		t.Errorf("reverse spread %g, wanted the 1%% fee plus the 5%% "+
			"move", back.Spread)
	}
}

// Without a current market rate nothing is quoted.
func TestNoMarketRateNoQuote(t *testing.T) {
	t.Parallel()

	cfg := usable()
	cfg.RateSource = RateSourceNeoxa
	m := newMarket(feedStart)
	mkt := testFeed(t, m)

	f := &fakeNode{synced: true, balance: 100_000_000_000}
	svc := serviceForMarket(t, cfg, f, mkt)
	svc.positionOf = f.deps().ChannelBalance
	fwd, _ := sidesOf(t, svc)

	if _, err := svc.pricer(fwd)(context.Background()); err == nil {
		t.Fatal("quoted before the market was read")
	}
	if !svc.heldForRate {
		t.Error("toBLAKE2b came up without a rate to convert its bounds at")
	}

	mkt.read(context.Background())
	m.advance(2 * time.Minute)
	if _, err := svc.pricer(fwd)(context.Background()); err == nil {
		t.Fatal("quoted on a reading two minutes old")
	}
}

// Just before paying, a swap whose payment in no longer covers the payout at the
// market's rate is given back; one that does is paid.
func TestThePriceIsCheckedBeforePaying(t *testing.T) {
	t.Parallel()

	cfg := usable()
	cfg.RateSource = RateSourceNeoxa
	m := newMarket(feedStart)
	mkt := testFeed(t, m)
	mkt.read(context.Background())

	f := &fakeNode{synced: true, balance: 100_000_000_000}
	svc := serviceForMarket(t, cfg, f, mkt)
	fwd, rev := sidesOf(t, svc)

	// toSHA256 at 0.006: 1,000,000 msat of BTCB2 is worth 6,000 msat of
	// SHA256 coin.
	sw := &driver.Swap{IncomingMsat: 1_000_000, OutgoingMsat: 5_900,
		MaxFeeMsat: 50}
	if ok, err := svc.priceCheck(fwd)(context.Background(), sw); err != nil ||
		!ok {

		t.Errorf("a swap still covered was refused: %v %v", ok, err)
	}
	sw.OutgoingMsat = 5_980
	if ok, _ := svc.priceCheck(fwd)(context.Background(), sw); ok {
		t.Error("paid a swap the market no longer covers")
	}

	// toBLAKE2b at 1/0.006: 6,000 msat of SHA256 coin is worth 1,000,000
	// msat of BTCB2.
	sw = &driver.Swap{IncomingMsat: 6_000, OutgoingMsat: 990_000,
		MaxFeeMsat: 3_000}
	if ok, err := svc.priceCheck(rev)(context.Background(), sw); err != nil ||
		!ok {

		t.Errorf("a toBLAKE2b swap still covered was refused: %v %v",
			ok, err)
	}
	sw.OutgoingMsat = 999_000
	if ok, _ := svc.priceCheck(rev)(context.Background(), sw); ok {
		t.Error("paid a toBLAKE2b swap the market no longer covers")
	}

	// No current rate is no decision.
	m.advance(5 * time.Minute)
	if _, err := svc.priceCheck(fwd)(context.Background(), sw); err == nil {
		t.Error("decided on a stale rate")
	}
}

// Following the market, the operator's SetRate is refused with a reason.
func TestSetRateIsRefusedWhileFollowingTheMarket(t *testing.T) {
	t.Parallel()

	srv, _, err := New(&Config{Enabled: true, ToSHA256: true,
		Deps: (&fakeNode{}).deps()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = srv.SetRate(context.Background(), &SetRateRequest{Rate: 0.006})
	if err == nil || !strings.Contains(err.Error(), "ratesource=fixed") {
		t.Fatalf("want SetRate refused, got %v", err)
	}
}

// serviceForMarket is serviceFor trading at mkt's rate.
func serviceForMarket(t *testing.T, cfg Config, f *fakeNode,
	mkt *feed) *service {

	t.Helper()

	cfg.Journal = filepath.Join(t.TempDir(), "swaps.journal")
	svc, err := newServiceWith(&cfg, NewLocal(f.deps()),
		remote(nil, nil, nil), mkt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.close)

	return svc
}

// While the bridge waits on its SHA256 node, Status still says where the rate
// comes from and whether the market can be read.
func TestStatusReportsTheMarketBeforeTheBridgeIsUp(t *testing.T) {
	t.Parallel()

	srv, _, err := New(&Config{Enabled: true, ToSHA256: true,
		Deps: (&fakeNode{synced: true}).deps()})
	if err != nil {
		t.Fatal(err)
	}
	m := newMarket(feedStart)
	m.now = time.Now()
	m.set(pairDirect, 0.006, 0.006, 0.00606)
	m.set(pairUSDC, 510, 509, 512)
	srv.market = testFeed(t, m)

	resp, err := srv.Status(context.Background(), &StatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.RateSource != RateSourceNeoxa || resp.FeeToSha256 != DefaultFee {
		t.Errorf("source %q, fee %g", resp.RateSource, resp.FeeToSha256)
	}
	if !strings.Contains(strings.Join(resp.Refusals, "; "),
		"reading the market") {

		t.Errorf("refusals %q", resp.Refusals)
	}

	srv.market.read(context.Background())
	resp, _ = srv.Status(context.Background(), &StatusRequest{})
	if resp.Rate != 0.006 || resp.RateCrossCheck == 0 {
		t.Errorf("rate %g, cross-check %g", resp.Rate,
			resp.RateCrossCheck)
	}
}
