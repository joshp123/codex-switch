package accounts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"time"

	"github.com/joshp123/codex-switch/internal/providerreply"
)

const openAIDailyUsageBreakdownPath = "/backend-api/wham/usage/daily-token-usage-breakdown"

// OpenAIModelDayCredits is the Codex credits one model at one speed used on
// one UTC day, as a percentage of the busiest day in the same read. OpenAI
// counts every client and machine, ChatGPT Work on the web included. The unit
// holds only within one read: OpenAI scales each response so that its busiest
// day totals 100, so a read starting on 1 September and one starting on 12
// September give the same day different values.
type OpenAIModelDayCredits struct {
	Date                string
	Model               string
	Fast                bool
	PercentOfBusiestDay float64
}

// ReadDailyModelCredits reads the per-model, per-speed credits for the UTC
// dates firstDate through lastDate. OpenAI fills in a day one to two days after
// it ends; until then the day is empty.
func (s *OpenAIDailyTokenSource) ReadDailyModelCredits(ctx context.Context, firstDate, lastDate time.Time) (string, []OpenAIModelDayCredits, error) {
	query := url.Values{}
	query.Set("start_date", firstDate.UTC().Format(time.DateOnly))
	query.Set("end_date", lastDate.UTC().Format(time.DateOnly))
	query.Set("group_by", "day")
	accountID, encoded, err := s.get(ctx, s.origin.ResolveReference(&url.URL{Path: openAIDailyUsageBreakdownPath, RawQuery: query.Encode()}).String())
	if err != nil {
		return "", nil, err
	}
	credits, err := decodeOpenAIModelDayCredits(encoded)
	return accountID, credits, err
}

func decodeOpenAIModelDayCredits(encoded []byte) ([]OpenAIModelDayCredits, error) {
	var wire struct {
		Units *string `json:"units"`
		Data  []struct {
			Date   string `json:"date"`
			Models []struct {
				Model   string   `json:"model"`
				Speed   string   `json:"speed"`
				Credits *float64 `json:"credits"`
			} `json:"models"`
		} `json:"data"`
	}
	if err := providerreply.Decode(bytes.NewReader(encoded), &wire); err != nil {
		return nil, fmt.Errorf("decode OpenAI daily usage breakdown: %w", err)
	}
	if wire.Units == nil || *wire.Units != "percent" {
		return nil, errors.New("OpenAI daily usage breakdown is not in percent of the busiest day")
	}
	var credits []OpenAIModelDayCredits
	for index, day := range wire.Data {
		if _, err := time.Parse(time.DateOnly, day.Date); err != nil {
			return nil, fmt.Errorf("OpenAI daily usage breakdown day %d has an invalid date", index+1)
		}
		for _, model := range day.Models {
			if model.Model == "" || model.Credits == nil || *model.Credits < 0 || math.IsInf(*model.Credits, 0) || math.IsNaN(*model.Credits) {
				return nil, fmt.Errorf("OpenAI daily usage breakdown for %s has invalid model credits", day.Date)
			}
			if model.Speed != "standard" && model.Speed != "fast" {
				return nil, fmt.Errorf("OpenAI daily usage breakdown for %s has a speed other than standard or fast", day.Date)
			}
			if *model.Credits == 0 {
				continue
			}
			credits = append(credits, OpenAIModelDayCredits{Date: day.Date, Model: model.Model, Fast: model.Speed == "fast", PercentOfBusiestDay: *model.Credits})
		}
	}
	return credits, nil
}
