package logql

import (
	"sync"

	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
)

// RangeVectorAggregator aggregates samples for a given range of samples.
// It receives the current milliseconds timestamp and the list of point within
// the range.
type RangeVectorAggregator func([]promql.Point) float64

// RangeVectorIterator iterates through a range of samples.
// To fetch the current vector use `At` with a `RangeVectorAggregator`.
type RangeVectorIterator interface {
	Next() bool
	At(aggregator RangeVectorAggregator) (int64, promql.Vector)
	Close() error
	Error() error
}

type rangeVectorIterator struct {
	iter                         SeriesIterator
	selRange, step, end, current int64
	window                       map[string]*promql.Series
	metrics                      map[string]labels.Labels
}

func newRangeVectorIterator(
	it SeriesIterator,
	selRange, step, start, end int64) *rangeVectorIterator {
	// forces at least one step.
	if step == 0 {
		step = 1
	}
	return &rangeVectorIterator{
		iter:     it,
		step:     step,
		end:      end,
		selRange: selRange,
		current:  start - step, // first loop iteration will set it to start
		window:   map[string]*promql.Series{},
		metrics:  map[string]labels.Labels{},
	}
}

func (r *rangeVectorIterator) Next() bool {
	// slides the range window to the next position
	r.current = r.current + r.step
	if r.current > r.end {
		return false
	}
	rangeEnd := r.current
	rangeStart := r.current - r.selRange
	// load samples
	r.popBack(rangeStart)
	r.load(rangeStart, rangeEnd)
	return true
}

func (r *rangeVectorIterator) Close() error {
	return r.iter.Close()
}

func (r *rangeVectorIterator) Error() error {
	return r.iter.Error()
}

// popBack removes all entries out of the current window from the back.
func (r *rangeVectorIterator) popBack(newStart int64) {
	// possible improvement: if there is no overlap we can just remove all.
	for fp := range r.window {
		lastPoint := 0
		remove := false
		for i, p := range r.window[fp].Points {
			if p.T <= newStart {
				lastPoint = i
				remove = true
				continue
			}
			break
		}
		if remove {
			r.window[fp].Points = r.window[fp].Points[lastPoint+1:]
		}
		if len(r.window[fp].Points) == 0 {
			s := r.window[fp]
			delete(r.window, fp)
			putSeries(s)
		}
	}
}

// load the next sample range window.
func (r *rangeVectorIterator) load(start, end int64) {
	for sample, hasNext := r.iter.Peek(); hasNext; sample, hasNext = r.iter.Peek() {
		if sample.TimestampNano > end {
			// not consuming the iterator as this belong to another range.
			return
		}
		// the lower bound of the range is not inclusive
		if sample.TimestampNano <= start {
			_ = r.iter.Next()
			continue
		}
		// adds the sample.
		var series *promql.Series
		var ok bool
		series, ok = r.window[sample.Labels]
		if !ok {
			var metric labels.Labels
			if metric, ok = r.metrics[sample.Labels]; !ok {
				var err error
				metric, err = parser.ParseMetric(sample.Labels)
				if err != nil {
					continue
				}
				r.metrics[sample.Labels] = metric
			}

			series = getSeries()
			series.Metric = metric
			r.window[sample.Labels] = series
		}
		p := promql.Point{
			T: sample.TimestampNano,
			V: sample.Value,
		}
		series.Points = append(series.Points, p)
		_ = r.iter.Next()
	}
}

func (r *rangeVectorIterator) At(aggregator RangeVectorAggregator) (int64, promql.Vector) {
	result := make([]promql.Sample, 0, len(r.window))
	// convert ts from nano to milli seconds as the iterator work with nanoseconds
	ts := r.current / 1e+6
	for _, series := range r.window {
		result = append(result, promql.Sample{
			Point: promql.Point{
				V: aggregator(series.Points),
				T: ts,
			},
			Metric: series.Metric,
		})
	}
	return ts, result
}

var seriesPool sync.Pool

func getSeries() *promql.Series {
	if r := seriesPool.Get(); r != nil {
		s := r.(*promql.Series)
		s.Points = s.Points[:0]
		return s
	}
	return &promql.Series{
		Points: make([]promql.Point, 0, 1024),
	}
}

func putSeries(s *promql.Series) {
	seriesPool.Put(s)
}
