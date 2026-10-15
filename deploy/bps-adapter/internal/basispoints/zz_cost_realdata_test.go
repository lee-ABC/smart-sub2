package basispoints

import (
	"encoding/csv"
	"math"
	"os"
	"strconv"
	"testing"
)

// Pricing is only useful if it agrees with what the main site bills, so it is
// checked against the main site's own records rather than against itself.
// Export them with the query in cost_rows.sql and point COST_ROWS_CSV at the
// file; without it there is nothing to check and the test says so.
//
// Columns: model, input_tokens, cache_read_tokens, output_tokens, total_cost.
// input_tokens is the uncached part, the way the main site stores it, so the
// prompt total the threshold applies to is input + cache_read.
func TestPricingAgreesWithTheMainSitesOwnRecords(t *testing.T) {
	path := os.Getenv("COST_ROWS_CSV")
	if path == "" {
		t.Skip("COST_ROWS_CSV not set")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	reader := csv.NewReader(file)
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 2 {
		t.Fatal("no rows")
	}

	cfg := defaultConfig()
	var checked, matched, unpriced int
	worst := 0.0
	mismatchByModel := map[string]int{}
	for _, row := range rows[1:] {
		if len(row) < 5 {
			continue
		}
		model := row[0]
		uncached, _ := strconv.ParseInt(row[1], 10, 64)
		cached, _ := strconv.ParseInt(row[2], 10, 64)
		output, _ := strconv.ParseInt(row[3], 10, 64)
		want, _ := strconv.ParseFloat(row[4], 64)

		price, ok := cfg.priceFor(model)
		if !ok {
			unpriced++
			continue
		}
		// The plugin reads Responses usage, where input_tokens includes the
		// cached part; the main site stores the two split apart.
		got := price.cost(tokenUsage{Input: uncached + cached, Cached: cached, Output: output})
		checked++
		if diff := math.Abs(got - want); diff <= 1e-9 {
			matched++
		} else {
			mismatchByModel[model]++
			worst = math.Max(worst, diff)
		}
	}

	t.Logf("checked %d rows: %d matched exactly, %d unpriced models, worst gap $%.9f",
		checked, matched, unpriced, worst)
	for model, count := range mismatchByModel {
		t.Logf("  mismatched: %s x%d", model, count)
	}
	if checked == 0 {
		t.Fatal("no rows could be priced")
	}
	// Some rows were billed at the priority tier, which the main site charges
	// at twice the rate and which this plugin never requests; the export
	// excludes them, so everything left has to match to the cent.
	if matched != checked {
		t.Errorf("%d of %d rows disagree with the main site", checked-matched, checked)
	}
}
