package metrics

import (
	"reflect"
	"testing"
)

func Test_parseStopwatchShowOutput(t *testing.T) {
	tests := []struct {
		name                string
		stopwatchShowOutput string
		want                map[string]stopwatchStatistics
	}{
		{
			name: "should return all metrics",
			stopwatchShowOutput: `Statistics for 'ovnnb_db_run'
  Total samples: 3618
  Maximum: 208 msec
  Minimum: 0 msec
  95th percentile: 52.887067 msec
  Short term average: 22.548798 msec
  Long term average: 26.117126 msec
Statistics for 'ovn-northd-loop'
  Total samples: 6269
  Maximum: 29999 msec
  Minimum: 0 msec
  95th percentile: 7726.066210 msec
  Short term average: 7778.877120 msec
  Long term average: 2740.125211 msec`,
			want: map[string]stopwatchStatistics{
				"ovnnb_db_run": {
					totalSamples:   "3618",
					min:            "0",
					max:            "208",
					percentile95th: "52.887067",
					shortTermAvg:   "22.548798",
					longTermAvg:    "26.117126",
				},
				"ovn-northd-loop": {
					totalSamples:   "6269",
					min:            "0",
					max:            "29999",
					percentile95th: "7726.066210",
					shortTermAvg:   "7778.877120",
					longTermAvg:    "2740.125211",
				},
			},
		},
		{
			name: "should return all metrics, even if nor ordered",
			stopwatchShowOutput: `Statistics for 'ovnnb_db_run'
  Short term average: 22.548798 msec
  Maximum: 208 msec
  95th percentile: 52.887067 msec
  Total samples: 3618
  Minimum: 0 msec
  Long term average: 26.117126 msec
Statistics for 'ovn-northd-loop'
  Total samples: 6269
  Maximum: 29999 msec
  Minimum: 0 msec
  95th percentile: 7726.066210 msec
  Short term average: 7778.877120 msec
  Long term average: 2740.125211 msec`,
			want: map[string]stopwatchStatistics{
				"ovnnb_db_run": {
					totalSamples:   "3618",
					min:            "0",
					max:            "208",
					percentile95th: "52.887067",
					shortTermAvg:   "22.548798",
					longTermAvg:    "26.117126",
				},
				"ovn-northd-loop": {
					totalSamples:   "6269",
					min:            "0",
					max:            "29999",
					percentile95th: "7726.066210",
					shortTermAvg:   "7778.877120",
					longTermAvg:    "2740.125211",
				},
			},
		},
		{
			name: "should be able to parse only one metric",
			stopwatchShowOutput: `Statistics for 'ovnnb_db_run'
  Maximum: 208 msec
  Minimum: 0 msec
  95th percentile: 52.887067 msec
  Total samples: 3618
  Short term average: 22.548798 msec
  Long term average: 26.117126 msec`,
			want: map[string]stopwatchStatistics{
				"ovnnb_db_run": {
					totalSamples:   "3618",
					min:            "0",
					max:            "208",
					percentile95th: "52.887067",
					shortTermAvg:   "22.548798",
					longTermAvg:    "26.117126",
				},
			},
		},
		{
			name: "should be able to parse output with missing metrics",
			stopwatchShowOutput: `Statistics for 'ovnnb_db_run'
  Maximum: 208 msec
  Total samples: 3618`,
			want: map[string]stopwatchStatistics{
				"ovnnb_db_run": {
					totalSamples: "3618",
					max:          "208",
				},
			},
		},
		{
			name:                "should be able to ignore empty output",
			stopwatchShowOutput: "",
			want:                map[string]stopwatchStatistics{},
		},
		{
			name:                "should be able to ignore wrong output",
			stopwatchShowOutput: "foo bar",
			want:                map[string]stopwatchStatistics{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseStopwatchShowOutput(tt.stopwatchShowOutput)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseStopwatchShowOutput() = %v, want %v", got, tt.want)
			}
		})
	}
}
