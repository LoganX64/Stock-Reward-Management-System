package marketdata

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/LoganX64/stocky-api/internal/marketcal"
	"github.com/LoganX64/stocky-api/internal/utils"
)

const (
	bhavcopyDefaultBaseURL = "https://nsearchives.nseindia.com"
	bhavcopyPathPrefix     = "/products/content/sec_bhavdata_full_"
	bhavcopyFileDateLayout = "02012006"
	bhavcopyAccept         = "text/csv,application/csv,text/plain,*/*"
)

// BhavcopyOptions configures the official NSE end-of-day provider.
type BhavcopyOptions struct {
	BaseURL string
	Mapper  *SymbolMapper
	HTTP    HTTPOptions
	Now     func() time.Time
}

// BhavcopyProvider implements DailyCloseProvider with the official NSE
// sec_bhavdata_full CSV archive. One file contains every NSE symbol for a
// trading date, so a batch request performs exactly one HTTP call.
type BhavcopyProvider struct {
	baseURL string
	mapper  *SymbolMapper
	http    *HTTPClient
	now     func() time.Time
}

var _ DailyCloseProvider = (*BhavcopyProvider)(nil)

// NewBhavcopyProvider creates an NSE-only bhavcopy provider. An empty BaseURL
// defaults to nsearchives.nseindia.com and a nil Mapper defaults to NSE.
func NewBhavcopyProvider(opts BhavcopyOptions) (*BhavcopyProvider, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		baseURL = bhavcopyDefaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("bhavcopy: invalid base URL %q: %w", baseURL, err)
	}
	if parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("bhavcopy: invalid base URL %q", baseURL)
	}

	mapper := opts.Mapper
	if mapper == nil {
		mapper, err = NewSymbolMapper(ExchangeNSE, "")
		if err != nil {
			return nil, err
		}
	}
	if mapper.Exchange() != ExchangeNSE {
		return nil, fmt.Errorf("bhavcopy: official EOD ingestion is NSE-only, not %s", mapper.Exchange())
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	httpOpts := opts.HTTP
	if httpOpts.Now == nil {
		httpOpts.Now = now
	}

	return &BhavcopyProvider{
		baseURL: baseURL,
		mapper:  mapper,
		http:    NewHTTPClient(SourceNSEBhavcopy, httpOpts),
		now:     now,
	}, nil
}

// Name returns the provider source name.
func (p *BhavcopyProvider) Name() string {
	return SourceNSEBhavcopy
}

// GetDailyCloses downloads the bhavcopy file for date once and returns the
// official close for every requested symbol that is present with SERIES=EQ.
// Missing symbols are omitted from the map; their absence is not an error.
func (p *BhavcopyProvider) GetDailyCloses(ctx context.Context, date time.Time, symbols []string) (map[string]Quote, error) {
	return p.getCloses(ctx, date, symbols, false)
}

// GetHistoricalDailyCloses is the backfill variant of GetDailyCloses. It uses
// the historical quote contract so an archive file older than the live
// seven-day window can still be imported. The returned close is raw: corporate
// actions are applied later by the adjusted views, never by this parser.
func (p *BhavcopyProvider) GetHistoricalDailyCloses(ctx context.Context, date time.Time, symbols []string) (map[string]Quote, error) {
	return p.getCloses(ctx, date, symbols, true)
}

func (p *BhavcopyProvider) getCloses(ctx context.Context, date time.Time, symbols []string, historical bool) (map[string]Quote, error) {
	if date.IsZero() {
		return nil, &ValidationError{
			Provider: SourceNSEBhavcopy,
			Field:    "date",
			Reason:   "market date is required",
		}
	}

	marketDate := marketcal.Default().Date(date)
	canonical := make([]string, 0, len(symbols))
	seen := make(map[string]struct{}, len(symbols))
	for _, raw := range symbols {
		symbol, err := p.mapper.BhavcopySymbol(raw)
		if err != nil {
			return nil, fmt.Errorf("bhavcopy: %w", err)
		}
		if _, ok := seen[symbol]; ok {
			continue
		}
		seen[symbol] = struct{}{}
		canonical = append(canonical, symbol)
	}
	if len(canonical) == 0 {
		return map[string]Quote{}, nil
	}

	endpoint := p.baseURL + bhavcopyPathPrefix + marketDate.Format(bhavcopyFileDateLayout) + ".csv"
	body, err := p.http.Get(ctx, endpoint, bhavcopyAccept, BhavcopyBodyLimit)
	if err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %w", ErrNoSession, err)
		}
		return nil, err
	}

	return p.parse(body, marketDate, canonical, historical)
}

func (p *BhavcopyProvider) parse(body []byte, marketDate time.Time, symbols []string, historical bool) (map[string]Quote, error) {
	body = bytes.TrimPrefix(body, []byte("\ufeff"))

	reader := csv.NewReader(bytes.NewReader(body))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, bhavcopyValidation("header", "response body is empty")
		}
		return nil, bhavcopyValidation("header", fmt.Sprintf("read CSV header: %v", err))
	}

	columns := make(map[string]int, len(header))
	for i, name := range header {
		columns[normalizeBhavcopyHeader(name)] = i
	}
	for _, required := range []string{"SYMBOL", "SERIES", "DATE1", "CLOSE_PRICE"} {
		if _, ok := columns[required]; !ok {
			return nil, bhavcopyValidation("header", fmt.Sprintf("missing required column %s", required))
		}
	}

	requested := make(map[string]struct{}, len(symbols))
	for _, symbol := range symbols {
		requested[symbol] = struct{}{}
	}

	asOf := marketcal.Default().SessionClose(marketDate).UTC()
	fetchedAt := p.now().UTC()
	result := make(map[string]Quote, len(symbols))

	for line := 2; ; line++ {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, bhavcopyValidation("csv", fmt.Sprintf("read row %d: %v", line, err))
		}
		if len(record) == 0 {
			continue
		}

		rawSymbol := bhavcopyField(record, columns["SYMBOL"])
		if rawSymbol == "" {
			continue
		}
		symbol := utils.NormalizeStockSymbol(rawSymbol)

		dateText := bhavcopyField(record, columns["DATE1"])
		rowDate, err := parseBhavcopyDate(dateText)
		if err != nil {
			return nil, bhavcopyValidation("DATE1", fmt.Sprintf("row %d: %v", line, err))
		}
		if !sameMarketDate(rowDate, marketDate) {
			return nil, bhavcopyValidation(
				"DATE1",
				fmt.Sprintf(
					"row %d: got %q, want %q",
					line,
					dateText,
					marketDate.Format("02-Jan-2006"),
				),
			)
		}

		if _, wanted := requested[symbol]; !wanted {
			continue
		}
		if !strings.EqualFold(bhavcopyField(record, columns["SERIES"]), "EQ") {
			continue
		}
		if _, exists := result[symbol]; exists {
			continue
		}

		closeText := bhavcopyField(record, columns["CLOSE_PRICE"])
		if closeText == "" || closeText == "-" {
			continue
		}
		price, err := decimal.NewFromString(closeText)
		if err != nil || !price.IsPositive() {
			continue
		}

		quote := Quote{
			Symbol:         symbol,
			ProviderSymbol: symbol,
			Price:          utils.RoundAmount(price),
			Currency:       CurrencyINR,
			Exchange:       ExchangeNSE,
			AsOf:           asOf,
			FetchedAt:      fetchedAt,
			Source:         SourceNSEBhavcopy,
		}
		validate := quote.Validate
		if historical {
			validate = quote.ValidateHistory
		}
		if err := validate(fetchedAt); err != nil {
			return nil, err
		}
		result[symbol] = quote
	}

	return result, nil
}

func bhavcopyValidation(field, reason string) error {
	return &ValidationError{
		Provider: SourceNSEBhavcopy,
		Field:    field,
		Reason:   reason,
	}
}

func normalizeBhavcopyHeader(name string) string {
	name = strings.TrimPrefix(strings.TrimSpace(name), "\ufeff")
	return strings.ToUpper(name)
}

func bhavcopyField(record []string, index int) string {
	if index < 0 || index >= len(record) {
		return ""
	}
	return strings.TrimSpace(record[index])
}

func parseBhavcopyDate(raw string) (time.Time, error) {
	for _, layout := range []string{"02-Jan-2006", "02-01-2006", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, raw, marketcal.Location()); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid date %q (want DD-Mon-YYYY)", raw)
}

func sameMarketDate(a, b time.Time) bool {
	ay, am, ad := a.In(marketcal.Location()).Date()
	by, bm, bd := b.In(marketcal.Location()).Date()
	return ay == by && am == bm && ad == bd
}
