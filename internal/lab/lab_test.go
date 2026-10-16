package lab

import (
	"context"
	"testing"
	"time"
)

func TestScenariosPreserveLastKnownGood(t *testing.T) {
	cases := []Request{
		{SourceID: "devfolio", Scenario: "unavailable"},
		{SourceID: "devfolio", Scenario: "forbidden"},
		{SourceID: "devfolio", Scenario: "rate_limited"},
		{SourceID: "devfolio", Scenario: "timeout"},
		{SourceID: "devfolio", Scenario: "changed_format"},
		{SourceID: "devfolio", Scenario: "changed_field"},
		{SourceID: "devfolio", Scenario: "invalid_event"},
		{SourceID: "hacklist", Scenario: "changed_format"},
		{SourceID: "hacklist", Scenario: "changed_field"},
		{SourceID: "hacklist", Scenario: "invalid_event"},
		{SourceID: "hacklist", Scenario: "empty"},
		{SourceID: "hackathons-rus", Scenario: "unavailable"},
		{SourceID: "hackathons-pro", Scenario: "changed_format"},
		{SourceID: "hackathon-radar", Scenario: "changed_field"},
		{SourceID: "devpost", Scenario: "empty"},
		{SourceID: "kaggle", Scenario: "unavailable"},
	}
	for _, input := range cases {
		t.Run(input.SourceID+"/"+input.Scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			report, err := Run(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if !report.Passed {
				t.Fatalf("scenario failed checks: %+v; adapter error: %s", report.Checks, report.Error)
			}
			if report.Before.EventCount != report.After.EventCount || report.After.Status != "failed" {
				t.Fatalf("last known good was not preserved: before=%+v after=%+v", report.Before, report.After)
			}
		})
	}
}

func TestRealAdaptersRecoverWithValidResponse(t *testing.T) {
	for _, sourceID := range []string{"devfolio", "hacklist"} {
		t.Run(sourceID, func(t *testing.T) {
			report, err := Run(context.Background(), Request{SourceID: sourceID, Scenario: "recovery"})
			if err != nil {
				t.Fatal(err)
			}
			if !report.Passed || report.RequestCount == 0 || report.After.EventCount <= report.Before.EventCount {
				t.Fatalf("adapter did not recover: %+v", report)
			}
		})
	}
}
