package marketdata

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/LoganX64/stocky-api/internal/utils"
)

// ErrSimulatedFailure is returned when the simulator is configured to inject
// failures. Real providers use their own errors.
var ErrSimulatedFailure = errors.New("marketdata: simulated provider failure")

// SimulatorOptions configures the local/offline quote provider.
type SimulatorOptions struct {
	Exchange     string
	SymbolSuffix string
	Now          func() time.Time
	Seed         int64
	Volatility   float64
	FailureRate  float64
	BasePrice    decimal.Decimal
}

// Simulator implements QuoteProvider with a random walk. It is selected
// explicitly with MARKET_DATA_PROVIDER=simulator and never performs network
// I/O.
type Simulator struct {
	mapper *SymbolMapper

	mu          sync.Mutex
	rng         *rand.Rand
	now         func() time.Time
	volatility  float64
	failureRate float64
	basePrice   decimal.Decimal
	prices      map[string]decimal.Decimal
}

// NewSimulator creates an NSE simulator with default settings.
func NewSimulator() *Simulator {
	s, err := NewSimulatorWithOptions(SimulatorOptions{})
	if err != nil {
		panic(fmt.Sprintf("marketdata: create simulator: %v", err))
	}
	return s
}

// NewSimulatorWithOptions creates a simulator with explicit exchange, clock,
// seed, volatility, failure injection, or base price.
func NewSimulatorWithOptions(opts SimulatorOptions) (*Simulator, error) {
	exchange := opts.Exchange
	if exchange == "" {
		exchange = ExchangeNSE
	}
	mapper, err := NewSymbolMapper(exchange, opts.SymbolSuffix)
	if err != nil {
		return nil, err
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	seed := opts.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}

	volatility := opts.Volatility
	if volatility == 0 {
		volatility = 0.05
	}
	if volatility < 0 || volatility > 0.5 {
		return nil, fmt.Errorf("marketdata: simulator volatility must be between 0 and 0.5")
	}

	if opts.FailureRate < 0 || opts.FailureRate > 1 {
		return nil, fmt.Errorf("marketdata: simulator failure rate must be between 0 and 1")
	}

	basePrice := opts.BasePrice
	if basePrice.IsZero() {
		basePrice = decimal.NewFromInt(1000)
	}
	if !basePrice.IsPositive() {
		return nil, fmt.Errorf("marketdata: simulator base price must be greater than zero")
	}

	return &Simulator{
		mapper:      mapper,
		rng:         rand.New(rand.NewSource(seed)),
		now:         now,
		volatility:  volatility,
		failureRate: opts.FailureRate,
		basePrice:   basePrice,
		prices:      make(map[string]decimal.Decimal),
	}, nil
}

// Name returns the provider source name.
func (s *Simulator) Name() string {
	return SourceSimulator
}

// Exchange returns the canonical exchange used by this simulator.
func (s *Simulator) Exchange() string {
	return s.mapper.Exchange()
}

// GetQuote returns a random-walk quote for symbol.
func (s *Simulator) GetQuote(ctx context.Context, symbol string) (Quote, error) {
	if err := ctx.Err(); err != nil {
		return Quote{}, err
	}

	canonical, err := s.mapper.Canonical(symbol)
	if err != nil {
		return Quote{}, err
	}
	providerSymbol, err := s.mapper.ProviderSymbol(canonical)
	if err != nil {
		return Quote{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failureRate > 0 && s.rng.Float64() < s.failureRate {
		return Quote{}, fmt.Errorf("%w: %s", ErrSimulatedFailure, canonical)
	}

	last, ok := s.prices[canonical]
	if !ok {
		last = s.basePrice
	}

	change := (s.rng.Float64()*2 - 1) * s.volatility
	price := utils.RoundAmount(last.Mul(decimal.NewFromFloat(1 + change)))
	if !price.IsPositive() {
		price = utils.RoundAmount(s.basePrice)
	}
	s.prices[canonical] = price

	now := s.now().UTC()
	quote := Quote{
		Symbol:         canonical,
		ProviderSymbol: providerSymbol,
		Price:          price,
		Currency:       CurrencyINR,
		Exchange:       s.mapper.Exchange(),
		AsOf:           now,
		FetchedAt:      now,
		Source:         SourceSimulator,
	}
	if err := quote.Validate(now); err != nil {
		return Quote{}, err
	}
	return quote, nil
}
