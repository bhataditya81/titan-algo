package broker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"titan-algo/internal/strategy"
)

// Historical Data Request
type historicalRequest struct {
	Exchange    string `json:"exchange"`
	SymbolToken string `json:"symboltoken"`
	Interval    string `json:"interval"`
	FromDate    string `json:"fromdate"`
	ToDate      string `json:"todate"`
}

// Historical Response
type historicalResponse struct {
	Status  bool            `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"` // Can be [][]interface{}
}

// FetchHistory fetches historical candle data from Angel One
// interval: "ONE_MINUTE", "FIVE_MINUTE", "ONE_DAY"
// days: number of days of history to fetch
func (a *AngelBroker) FetchHistory(symbol, interval string, days int) ([]strategy.Candle, error) {
	// 1. Get Instrument Token
	inst, err := a.instruments.GetInstrument(symbol)
	if err != nil {
		return nil, fmt.Errorf("unknown symbol %s: %v", symbol, err)
	}

	// 2. Prepare Dates (Angel Format: 2006-01-02 15:04)
	to := time.Now()
	from := to.AddDate(0, 0, -days)

	// Angel requires basic format without timezone for historical API usually
	// Format: "2021-02-08 09:15"
	const angelDateFormat = "2006-01-02 15:04"
	fromStr := from.Format(angelDateFormat)
	toStr := to.Format(angelDateFormat)

	log.Printf("📥 Fetching %s history (%s) for %s [%s] from %s to %s", interval, inst.ExchSeg, symbol, inst.Token, fromStr, toStr)

	// 3. Prepare Payload
	payload := historicalRequest{
		Exchange:    inst.ExchSeg,
		SymbolToken: inst.Token,
		Interval:    interval,
		FromDate:    fromStr,
		ToDate:      toStr,
	}

	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	// 4. Execute Request
	// Note: using internal baseURL + path
	url := "https://apiconnect.angelone.in/rest/secure/angelbroking/historical/v1/getCandleData"
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-PrivateKey", a.apiKey) // Accessing unexported field (same package)
	req.Header.Set("Authorization", "Bearer "+a.accessToken)
	req.Header.Set("X-ClientLocalIP", "127.0.0.1")
	req.Header.Set("X-ClientPublicIP", "127.0.0.1")
	req.Header.Set("X-MACAddress", "00:00:00:00:00:00")
	req.Header.Set("X-UserType", "USER")
	req.Header.Set("X-SourceID", "WEB")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var histResp historicalResponse
	if err := json.Unmarshal(body, &histResp); err != nil {
		return nil, fmt.Errorf("json parse error: %v | Body: %s", err, string(body))
	}

	if !histResp.Status {
		return nil, fmt.Errorf("API error: %s", histResp.Message)
	}

	// 5. Parse Data
	// Data is [][]interface{}
	var rows [][]interface{}
	if err := json.Unmarshal(histResp.Data, &rows); err != nil {
		return nil, fmt.Errorf("failed to parse data rows: %v", err)
	}

	log.Printf("📊 Received %d candles", len(rows))

	var candles []strategy.Candle

	for _, row := range rows {
		// Expected: [Timestamp(string), Open, High, Low, Close, Volume]
		if len(row) < 6 {
			continue
		}

		// Helper to safely get float
		getFloat := func(v interface{}) float64 {
			switch val := v.(type) {
			case float64:
				return val
			case string:
				f, _ := strconv.ParseFloat(val, 64)
				return f
			default:
				return 0
			}
		}

		// Parse Timestamp
		tsStr, ok := row[0].(string)
		if !ok {
			continue // Skip invalid
		}
		// Angel Hist format: "2021-02-08T09:15:00+05:30" (RFC3339)
		ts, err := time.Parse(time.RFC3339, tsStr)
		if err != nil {
			// Fallback: try other formats if needed, but usually RFC3339
			log.Printf("Warning provided timestamp format mismatch: %s", tsStr)
			continue
		}

		c := strategy.Candle{
			Time:   ts,
			Open:   getFloat(row[1]),
			High:   getFloat(row[2]),
			Low:    getFloat(row[3]),
			Close:  getFloat(row[4]),
			Volume: int64(getFloat(row[5])),
		}

		// Note: The SniperStrategy relies on ORDER of candles. Append order is chronological.
		candles = append(candles, c)
	}

	return candles, nil
}
