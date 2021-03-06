// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package ts

import (
	"container/heap"
	"context"
	"fmt"
	"math"
	"sort"
	"time"
	"unsafe"

	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/ts/tspb"
	"github.com/cockroachdb/cockroach/pkg/util/mon"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/pkg/errors"
)

// Compute the size of various structures to use when tracking memory usage.
var (
	sizeOfDataSpan       = int64(unsafe.Sizeof(dataSpan{}))
	sizeOfCalibratedData = int64(unsafe.Sizeof(calibratedData{}))
	sizeOfTimeSeriesData = int64(unsafe.Sizeof(roachpb.InternalTimeSeriesData{}))
	sizeOfSample         = int64(unsafe.Sizeof(roachpb.InternalTimeSeriesSample{}))
	sizeOfDataPoint      = int64(unsafe.Sizeof(tspb.TimeSeriesDatapoint{}))
)

// calibratedData is used to calibrate an InternalTimeSeriesData object for
// use in a dataSpan.  This is accomplished by computing a constant offset
// adjustment which adjusts each Sample offset to be relative to the start time
// of the dataSpan, rather than the start time of the InternalTimeSeriesData.
type calibratedData struct {
	roachpb.InternalTimeSeriesData
	offsetAdjustment int32
}

// offsetAt returns the calibrated offset for the Sample at the supplied index
// in the InternalTimeSeriesData's Samples collection.
func (rdc *calibratedData) offsetAt(idx int) int32 {
	return rdc.Samples[idx].Offset + rdc.offsetAdjustment
}

// dataSpan is used to construct a monolithic view of a single time series over
// an arbitrary time span. The actual data in a span may be stored in multiple
// instances of InternalTimeSeriesData.
type dataSpan struct {
	startNanos  int64
	sampleNanos int64
	datas       []calibratedData
}

// addData adds an InternalTimeSeriesData object into this dataSpan, normalizing
// it to a calibratedData object in the process.
func (ds *dataSpan) addData(data roachpb.InternalTimeSeriesData) error {
	if data.SampleDurationNanos != ds.sampleNanos {
		return errors.Errorf("data added to dataSpan with mismatched sample duration period")
	}

	// Reject data if there are no samples.
	if len(data.Samples) == 0 {
		return nil
	}

	// Calculate an offset adjustment which normalizes the supplied data into
	// the dataSpan's time period.
	adjustment := (data.StartTimestampNanos - ds.startNanos) / ds.sampleNanos
	rd := calibratedData{
		InternalTimeSeriesData: data,
		offsetAdjustment:       int32(adjustment),
	}

	// If the supplied data does not occur after all previously added data,
	// return an error.
	if len(ds.datas) > 0 {
		last := ds.datas[len(ds.datas)-1]
		if rd.offsetAt(0) <= last.offsetAt(len(last.Samples)-1) {
			return errors.Errorf("data must be added to dataSpan in chronological order")
		}
	}

	ds.datas = append(ds.datas, rd)
	return nil
}

// extractFn is a function which extracts a float64 value from a time series
// sample.
type extractFn func(roachpb.InternalTimeSeriesSample) float64

// dataSpanIterator is used to iterate through the samples in a dataSpan.
// Samples are spread across multiple InternalTimeSeriesData objects; this
// iterator thus maintains a two-level index to point to a unique sample.
type dataSpanIterator struct {
	dataSpan
	dataIdx   int       // Index of InternalTimeSeriesData which contains current Sample
	sampleIdx int       // Index of current Sample within InternalTimeSeriesData
	valid     bool      // True if this iterator points to a valid Sample
	extractFn extractFn // Function to extract float64 values from samples
}

// newDataSpanIterator creates an iterator over the real data present in the
// supplied data span. The iterator is initialized to the requested offset if a
// real data point exists at that offset; otherwise, it is initialized to the
// smallest offset which is greater than the requested offset.
//
// If the requested offset is greater than all points in the dataSpan, the
// returned dataSpanIterator is initialized as if it had been advanced beyond
// the last index in the dataSpan; calling retreat() on this iterator will place
// it on the last datapoint.
func newDataSpanIterator(ds dataSpan, offset int32, extractFn extractFn) dataSpanIterator {
	// If there is no data at all, iterator is completely invalid.
	if len(ds.datas) == 0 {
		return dataSpanIterator{}
	}

	// Use a binary search to find the data span which should contain the offset.
	dataIdx := sort.Search(len(ds.datas), func(i int) bool {
		data := ds.datas[i]
		return data.offsetAt(len(data.Samples)-1) >= offset
	})

	if dataIdx == len(ds.datas) {
		return dataSpanIterator{
			dataSpan:  ds,
			dataIdx:   len(ds.datas) - 1,
			sampleIdx: len(ds.datas[len(ds.datas)-1].Samples),
			valid:     false,
			extractFn: extractFn,
		}
	}

	// Use a binary search to find the sample with the smallest offset >= the
	// target offset.
	data := ds.datas[dataIdx]
	sampleIdx := sort.Search(len(data.Samples), func(i int) bool {
		return data.offsetAt(i) >= offset
	})

	return dataSpanIterator{
		dataSpan:  ds,
		dataIdx:   dataIdx,
		sampleIdx: sampleIdx,
		valid:     true,
		extractFn: extractFn,
	}
}

// value returns a float64 value by applying extractFn to the
// InternalTimeSeriesSample value currently pointed to by this iterator.
func (dsi dataSpanIterator) value() float64 {
	if !dsi.valid {
		panic(fmt.Sprintf("value called on invalid dataSpanIterator: %v", dsi))
	}
	return dsi.extractFn(dsi.datas[dsi.dataIdx].Samples[dsi.sampleIdx])
}

// sample returns the InternalTimeSeriesSample which represents the current
// sample.
func (dsi dataSpanIterator) sample() roachpb.InternalTimeSeriesSample {
	if !dsi.valid {
		panic(fmt.Sprintf("sample called on invalid dataSpanIterator: %v", dsi))
	}
	return dsi.datas[dsi.dataIdx].Samples[dsi.sampleIdx]
}

// return the offset of the current sample, relative to the start of the
// dataSpan.
func (dsi dataSpanIterator) offset() int32 {
	if !dsi.valid {
		panic(fmt.Sprintf("offset called on invalid dataSpanIterator: %v", dsi))
	}
	data := dsi.datas[dsi.dataIdx]
	return data.offsetAt(dsi.sampleIdx)
}

// return the real timestamp represented by the current sample. The timestamp
// is located at the beginning of the sample period.
func (dsi dataSpanIterator) timestamp() int64 {
	if !dsi.valid {
		panic(fmt.Sprintf("timestamp called on invalid dataSpanIterator: %v", dsi))
	}
	return dsi.startNanos + (int64(dsi.offset()) * dsi.sampleNanos)
}

// advance moves the iterator to point to the next Sample.
func (dsi *dataSpanIterator) advance() {
	if !dsi.valid {
		// Three possible scenarios for an invalid iterator:
		// - iterator was never valid (no data)
		// - iterator was advanced past the last index
		// - iterator was retreated past the earliest index
		// We can distinguish these based on the value of of sampleIdx. In the
		// case where we are ahead of the earliest index, we advance sampleIdx
		// and revalidate the index.
		if dsi.sampleIdx < 0 {
			dsi.valid = true
			dsi.sampleIdx++
		}
		return
	}
	data := dsi.datas[dsi.dataIdx]
	switch {
	case dsi.sampleIdx+1 < len(data.Samples):
		dsi.sampleIdx++
	case dsi.dataIdx+1 < len(dsi.datas):
		dsi.dataIdx++
		data = dsi.datas[dsi.dataIdx]
		dsi.sampleIdx = 0
	default:
		// Iterator is at the end of available data. Increment sample index and
		// invalidate.
		dsi.sampleIdx++
		dsi.valid = false
		return
	}
}

// retreat moves the iterator to the previous Sample.
func (dsi *dataSpanIterator) retreat() {
	if !dsi.valid {
		// Three possible scenarios for an invalid iterator:
		// - iterator was never valid (no data)
		// - iterator was advanced past the last index
		// - iterator was retreated past the earliest index
		// We can distinguish these based on the value of of sampleIdx. In the
		// case where we are after the lastest index, we retreat sampleIdx
		// and revalidate the index.
		if dsi.sampleIdx > 0 {
			dsi.valid = true
			dsi.sampleIdx--
		}
		return
	}
	data := dsi.datas[dsi.dataIdx]
	switch {
	case dsi.sampleIdx > 0:
		dsi.sampleIdx--
	case dsi.dataIdx > 0:
		dsi.dataIdx--
		data = dsi.datas[dsi.dataIdx]
		dsi.sampleIdx = len(data.Samples) - 1
	default:
		// Iterator is at the end of available data. Decrement sample index and
		// invalidate.
		dsi.sampleIdx--
		dsi.valid = false
		return
	}
}

func (dsi *dataSpanIterator) isValid() bool {
	return dsi.valid
}

// downsampleFn is a function which computes a single float64 value from a set
// of InternalTimeSeriesSample objects.
type downsampleFn func(...roachpb.InternalTimeSeriesSample) float64

// downsamplingIterator behaves like a dataSpanIterator, but converts data to a
// longer sample period through downsampling. Each offset of the downsampling
// iterator covers multiple offsets of the underlying data, according to a
// constant sampling factor. When a value is requested from this iterator, it is
// computed from the matching underlying offsets using a downsampling function.
//
// In the case where sampleFactor is 1, all operations are passed directly to a
// single underlying dataSpanIterator. Similar behavior have been accomplished
// by creating a common interface between downsamplingIterator and
// dataSpanIterator; however, using this technique means that no pointers are
// necessary, and all iterator types can be used without allocations.
type downsamplingIterator struct {
	sampleNanos    int64
	sampleFactor   int32
	underlyingData dataSpan
	start          dataSpanIterator
	end            dataSpanIterator
	downsampleFn   downsampleFn
}

// newDownsamplingIterator creates an iterator over given dataSpan. The iterator
// is initialized to the requested offset if any real samples are present in the
// underlying dataSpan which match that offset; otherwise, it is initialized to
// the smallest offset with data which is greater than the requested offset.
//
// If the requested offset is greater than all points in the dataSpan, the
// returned dataSpanIterator is initialized as if it had been advanced beyond
// the last index in the dataSpan; calling retreat() on this iterator will place
// it on the last datapoint.
func newDownsamplingIterator(
	ds dataSpan, offset int32, sampleNanos int64, extractFn extractFn, downsampleFn downsampleFn,
) downsamplingIterator {
	dsi := downsamplingIterator{
		sampleNanos:    sampleNanos,
		sampleFactor:   int32(sampleNanos / ds.sampleNanos),
		underlyingData: ds,
		downsampleFn:   downsampleFn,
	}
	if dsi.sampleFactor == 1 {
		dsi.start = newDataSpanIterator(ds, offset, extractFn)
		return dsi
	}

	underlyingOffset := offset * dsi.sampleFactor
	dsi.start = newDataSpanIterator(ds, underlyingOffset, extractFn)
	dsi.computeEnd()
	return dsi
}

// advance moves the iterator to the next downsampling offset for which data
// is present.
func (dsi *downsamplingIterator) advance() {
	if dsi.sampleFactor == 1 {
		dsi.start.advance()
		return
	}

	dsi.start = dsi.end
	if dsi.start.valid {
		dsi.computeEnd()
	}
}

// retreat moves the iterator to the previous downsampling offset for which data
// is present.
func (dsi *downsamplingIterator) retreat() {
	if dsi.sampleFactor == 1 {
		dsi.start.retreat()
		return
	}

	dsi.end = dsi.start
	dsi.start.retreat()
	if dsi.start.valid {
		startOffset := dsi.start.offset() - (dsi.start.offset() % dsi.sampleFactor)
		// Adjustment for negative offsets; the modulo math rounds negative
		// numbers up to the the next offset boundary, so subtract the
		// sampleFactor.
		if dsi.start.offset() < 0 {
			startOffset -= dsi.sampleFactor
		}
		dsi.start = newDataSpanIterator(dsi.underlyingData, startOffset, dsi.start.extractFn)
	}
}

// isValid returns true if this iterator points to valid data.
func (dsi *downsamplingIterator) isValid() bool {
	return dsi.start.valid
}

// offset returns the current offset of the iterator from the start of the
// underlying dataSpan. This offset is in terms of the sampleNanos of the
// iterator, not of the dataSpan; they are related to dataSpan offsets by
// sampleFactor.
func (dsi *downsamplingIterator) offset() int32 {
	return dsi.start.offset() / dsi.sampleFactor
}

// timestamp returns the timestamp corresponding to the current offset of the iterator.
// The returned timestamp marks the beginning of the sample period.
func (dsi *downsamplingIterator) timestamp() int64 {
	return dsi.start.dataSpan.startNanos + (int64(dsi.offset()) * dsi.sampleNanos)
}

// value returns a downsampled valued, computed using downsampleFn, based on the
// corresponding higher-resolution samples in the underlying dataSpan.
func (dsi *downsamplingIterator) value() float64 {
	if dsi.sampleFactor == 1 {
		return dsi.start.value()
	}

	end := dsi.end
	samples := make([]roachpb.InternalTimeSeriesSample, 0, dsi.sampleFactor)
	for iter := dsi.start; iter.valid && (!end.valid || iter.offset() != end.offset()); iter.advance() {
		samples = append(samples, iter.sample())
	}
	return dsi.downsampleFn(samples...)
}

func (dsi *downsamplingIterator) computeEnd() {
	if !dsi.start.valid {
		return
	}
	endOffset := (dsi.offset() + 1) * dsi.sampleFactor
	dsi.end = newDataSpanIterator(dsi.underlyingData, endOffset, dsi.start.extractFn)
}

// interpolatingIterator is used to iterate over offsets within a dataSpan. The
// iterator can provide sample values for any offset, even if there is no actual
// sample in the dataSpan at that offset.
//
// Values for missing offsets are computed using linear interpolation from the
// nearest real samples preceding and following the missing offset.
//
// If the maxDistance option is set to a value greater than zero, values will
// not be interpolated between real data points which have a difference in
// offset greater than maxDistance.
//
// If the derivative option is set, value() will return the derivative of the
// series at the current offset in units per offset.
type interpolatingIterator struct {
	offset      int32                // Current offset within dataSpan
	maxDistance int32                // Maximum distance between real values for interpolation.
	nextReal    downsamplingIterator // Next sample with an offset >= iterator's offset
	prevReal    downsamplingIterator // Prev sample with offset < iterator's offset
	derivative  tspb.TimeSeriesQueryDerivative
}

// newInterpolatingIterator returns an interpolating iterator for the given
// dataSpan. The iterator is initialized to position startOffset. Values
// returned by the iterator will be generated from samples using the supplied
// downsampleFn.
func newInterpolatingIterator(
	ds dataSpan,
	startOffset int32,
	sampleNanos int64,
	maxDistance int32,
	extractFn extractFn,
	downsampleFn downsampleFn,
	derivative tspb.TimeSeriesQueryDerivative,
) interpolatingIterator {
	if len(ds.datas) == 0 {
		return interpolatingIterator{}
	}

	nextReal := newDownsamplingIterator(ds, startOffset, sampleNanos, extractFn, downsampleFn)
	iterator := interpolatingIterator{
		offset:      startOffset,
		maxDistance: maxDistance,
		nextReal:    nextReal,
		derivative:  derivative,
	}

	prevReal := nextReal
	prevReal.retreat()
	if prevReal.isValid() {
		iterator.prevReal = prevReal
	}

	return iterator
}

// advanceTo advances the iterator to the supplied offset.
func (ii *interpolatingIterator) advanceTo(offset int32) {
	ii.offset = offset
	// Advance real iterators until nextReal has offset >= the interpolated
	// offset.
	for ii.nextReal.isValid() && ii.nextReal.offset() < ii.offset {
		ii.prevReal = ii.nextReal
		ii.nextReal.advance()
	}
}

// isValid returns true if this interpolatingIterator still points to valid data.
func (ii *interpolatingIterator) isValid() bool {
	return ii.nextReal.isValid()
}

// midTimestamp returns a timestamp at the middle of the current offset's sample
// period. The middle of the sample period has been chosen in order to minimize
// the possible distance from the returned timestamp and the timestamp of the
// real measurements used to compute its value.
func (ii *interpolatingIterator) midTimestamp() int64 {
	if !ii.isValid() {
		panic(fmt.Sprintf("midTimestamp called on invalid interpolatingIterator: %v", ii))
	}
	dsi := ii.nextReal
	return dsi.underlyingData.startNanos + (int64(ii.offset) * dsi.sampleNanos) + (dsi.sampleNanos / 2)
}

// value returns the value at the current offset of this iterator, or the
// derivative at the current offset. The boolean parameter indicates if the
// returned value represents a valid value; if false, the iterator is unable
// to return a value at the current offset.
func (ii *interpolatingIterator) value() (float64, bool) {
	if !ii.isValid() {
		return 0, false
	}
	isDerivative := ii.derivative != tspb.TimeSeriesQueryDerivative_NONE
	if !isDerivative && ii.nextReal.offset() == ii.offset {
		return ii.nextReal.value(), true
	}
	// Cannot interpolate or compute derivative if previous value is invalid.
	if !ii.prevReal.isValid() {
		return 0, false
	}

	// Do not interpolate if hole is greater than maxDistance.
	if ii.maxDistance > 0 && (ii.nextReal.offset()-ii.prevReal.offset()) > ii.maxDistance {
		return 0, false
	}

	// Linear interpolation of derivative or value at the current offset.
	off := float64(ii.offset)
	nextVal := ii.nextReal.value()
	nextOff := float64(ii.nextReal.offset())
	prevVal := ii.prevReal.value()
	prevOff := float64(ii.prevReal.offset())

	// Note: The derivative value could be factored out into a variable and used
	// for all of these returns, but doing so seems to introduce a greater
	// incidence of floating point artifacts in the non-derivative case due to
	// the order of operations.
	if !isDerivative {
		return prevVal + (nextVal-prevVal)*(off-prevOff)/(nextOff-prevOff), true
	}
	deriv := (nextVal - prevVal) / (nextOff - prevOff)
	if ii.derivative == tspb.TimeSeriesQueryDerivative_NON_NEGATIVE_DERIVATIVE &&
		deriv < 0 {
		return 0, true
	}
	return deriv, true
}

// An aggregatingIterator jointly advances multiple interpolatingIterators,
// visiting precisely those offsets for which at least one of the underlying
// interpolating iterators has a real (that is, non-interpolated) value.
//
// All valid iterators in the set will have the same offset at all times. During
// advancement, the next offset is chosen by finding the individual iterator
// with the lowest value of nextReal.offset; in other words, the
// aggregatingIterator will visit each possible offset in sequence, skipping
// offsets for which *no* interpolatingIterators have real data.  If even a
// single iterator has real data at an offset, that offset will eventually be
// visited.
//
// In order to facilitate finding the interpolatingIterator with the lowest
// underlying real offset, the set is organized as a min heap using Go's heap
// package.
type aggregatingIterator []interpolatingIterator

// Len returns the length of the aggregatingIterator; needed by heap.Interface.
func (ai aggregatingIterator) Len() int {
	return len(ai)
}

// Swap swaps the values at the two given indices; needed by heap.Interface.
func (ai aggregatingIterator) Swap(i, j int) {
	ai[i], ai[j] = ai[j], ai[i]
}

// Less determines if the iterator at the first supplied index in the
// aggregatingIterator is "Less" than the iterator at the second index; need by
// heap.Interface.
//
// An interpolatingIterator is considered "less" than another if its underlying
// *real* offset points to an earlier offset.
func (ai aggregatingIterator) Less(i, j int) bool {
	thisNext, otherNext := ai[i].nextReal, ai[j].nextReal
	if !(thisNext.isValid() || otherNext.isValid()) {
		return false
	}
	if !thisNext.isValid() {
		return false
	}
	if !otherNext.isValid() {
		return true
	}
	return thisNext.offset() < otherNext.offset()
}

// Push pushes an element into the aggregatingIterator heap; needed by
// heap.Interface
func (ai *aggregatingIterator) Push(x interface{}) {
	// Push and Pop use pointer receivers because they modify the slice's length,
	// not just its contents.
	*ai = append(*ai, x.(interpolatingIterator))
}

// Pop removes the minimum element from the aggregatingIterator heap; needed by
// heap.Interface.
func (ai *aggregatingIterator) Pop() interface{} {
	old := *ai
	n := len(old)
	x := old[n-1]
	*ai = old[0 : n-1]
	return x
}

// isValid returns true if at least one iterator in the set is still valid. This
// method only works if init() has already been called on the set.
func (ai aggregatingIterator) isValid() bool {
	return len(ai) > 0 && ai[0].isValid()
}

// anyInvalid returns true if any component interpolatingIterator is invalid.
func (ai aggregatingIterator) anyInvalid() bool {
	for i := range ai {
		if !ai[i].isValid() {
			return true
		}
	}
	return false
}

// init initializes the aggregatingIterator. This method moves all component
// iterators to the first offset for which *any* interpolatingIterator in the
// set has *real* data.
func (ai aggregatingIterator) init() {
	heap.Init(&ai)
	if !ai.isValid() {
		return
	}
	if ai[0].nextReal.offset() > 0 {
		ai.advance()
	}
}

// advance advances each iterator in the set to the next value for which *any*
// interpolatingIterator has a real value.
func (ai aggregatingIterator) advance() {
	if !ai.isValid() {
		return
	}

	// All iterators in the set currently point to the same offset. Advancement
	// begins by pre-advancing any iterators that have a real value for the
	// current offset.
	current := ai[0].offset
	for ai[0].offset == current {
		ai[0].advanceTo(current + 1)
		heap.Fix(&ai, 0)
	}

	// It is possible that all iterators are now invalid.
	if !ai.isValid() {
		return
	}

	// The iterator in position zero now has the lowest value for
	// nextReal.offset - advance all iterators to that offset.
	min := ai[0].nextReal.offset()
	for i := range ai {
		ai[i].advanceTo(min)
	}
	heap.Init(&ai)
}

// timestamp returns a timestamp for the current offset of the
// aggregatingIterator. Offsets should be converted into timestamps before
// returning them as part of a query result.
func (ai aggregatingIterator) timestamp() int64 {
	if !ai.isValid() {
		return 0
	}
	return ai[0].midTimestamp()
}

// validSumAndCount returns the sum value and count of all component
// interpolatingIterators that currently have valid values.
func (ai aggregatingIterator) validSumAndCount() (float64, int) {
	var sum float64
	var count int
	for i := range ai {
		value, valid := ai[i].value()
		if valid {
			sum += value
			count++
		}
	}
	return sum, count
}

// sum returns the sum of the current values of the interpolatingIterators being
// aggregated. The boolean parameter indicates if the returned value is valid;
// it will be invalid if all component iterators are invalid.
func (ai aggregatingIterator) sum() (float64, bool) {
	sum, count := ai.validSumAndCount()
	if count == 0 {
		return 0.0, false
	}
	return sum, true
}

// avg returns the average of the current values of the interpolatingIterators
// being aggregated. The boolean parameter indicates if the returned value is
// valid; it will be invalid if all component iterators are invalid.
func (ai aggregatingIterator) avg() (float64, bool) {
	sum, count := ai.validSumAndCount()
	if count == 0 {
		return 0.0, false
	}
	return sum / float64(count), true
}

// max return the maximum value of the current values of the
// interpolatingIterators being aggregated. The boolean parameter indicates if
// the returned value is valid; it will be invalid if all component iterators
// are invalid.
func (ai aggregatingIterator) max() (float64, bool) {
	max := math.Inf(-1)
	anyValid := false
	for i := range ai {
		value, valid := ai[i].value()
		anyValid = anyValid || valid
		if valid && value > max {
			max = value
		}
	}
	return max, anyValid
}

// min return the minimum value of the current values of the
// interpolatingIterators being aggregated. The boolean parameter indicates if
// the returned value is valid; it will be invalid if all component iterators
// are invalid.
func (ai aggregatingIterator) min() (float64, bool) {
	min := math.Inf(1)
	anyValid := false
	for i := range ai {
		value, valid := ai[i].value()
		anyValid = anyValid || valid
		if valid && value < min {
			min = value
		}
	}
	return min, anyValid
}

// makeLeadingEdgeFilter returns a function that filters data points near the
// leading edge of time that are "incomplete". Any data point with a timestamp
// later than the supplied "leading edge" timestamp (leadingEdgeNanos) is
// required to have a valid contribution from all sources being aggregated.
//
// A detailed explanation of why this is necessary: New time series data points
// are, in typical usage, always added at the current time; however, due to the
// curiosities of clock skew, it is a common occurrence for the most recent data
// point to be available for some sources, but not from others. For queries
// which aggregate from multple sources, this can lead to a situation where a
// persistent and precipitous dip at very end of data graphs. This happens
// because the most recent point only represents the aggregation of a subset of
// sources, even though the missing sources are not actually offline, they are
// simply slightly delayed in reporting.
//
// Linear interpolation can handle small gaps in the middle of data, but it does
// not work in this case as the current time is later than any data available
// from the missing sources.
//
// In this case, we can assume that a missing data point will be added soon, and
// instead do *not* return the partially aggregated data point to the client.
func (ai aggregatingIterator) makeLeadingEdgeFilter(
	fn func(aggregatingIterator) (float64, bool), leadingEdgeNanos int64,
) func() (float64, bool) {
	return func() (float64, bool) {
		value, valid := fn(ai)
		if valid {
			// Only check if the iterator's current offset is later than the cutoff.
			timestampNanos := ai.timestamp()
			if timestampNanos > leadingEdgeNanos {
				// Filter if any component iterators are invalid.
				if ai.anyInvalid() {
					valid = false
				}
			}
		}
		return value, valid
	}
}

// getMaxTimespan computes the longest timespan that can be safely queried while
// remaining within the given memory budget. Inputs are the resolution of data
// being queried, the budget, the estimated number of sources, and the
// interpolation limit being used for the query.
func getMaxTimespan(
	r Resolution, memoryBudget, estimatedSourceCount, interpolationLimitNanos int64,
) (int64, error) {
	slabDuration := r.SlabDuration()

	// Size of slab is the size of a completely full data slab for the supplied
	// data resolution.
	sizeOfSlab := sizeOfTimeSeriesData + (slabDuration/r.SampleDuration())*sizeOfSample

	// InterpolationBuffer is the number of slabs outside of the query range
	// needed to satisfy the interpolation limit. Extra slabs may be queried
	// on both sides of the target range.
	interpolationBufferOneSide :=
		int64(math.Ceil(float64(interpolationLimitNanos) / float64(slabDuration)))

	interpolationBuffer := interpolationBufferOneSide * 2

	// If the (interpolation buffer timespan - interpolation limit) is less than
	// half of a slab, then it is possible for one additional slab to be queried
	// that would not have otherwise been queried. This can occur when the queried
	// timespan does not start on an even slab boundary.
	if (interpolationBufferOneSide*slabDuration)-interpolationLimitNanos < slabDuration/2 {
		interpolationBuffer++
	}

	// The number of slabs that can be queried safely is perSeriesMem/sizeOfSlab,
	// less the interpolation buffer.
	perSourceMem := memoryBudget / estimatedSourceCount
	numSlabs := perSourceMem/sizeOfSlab - interpolationBuffer
	if numSlabs <= 0 {
		return 0, fmt.Errorf("insufficient memory budget to attempt query")
	}

	return numSlabs * slabDuration, nil
}

// QueryMemoryConstrained executes a query while attempting to limit the maximum
// amount of memory needed at one time. It accomplishes this by breaking the
// query into multiple "chunks" as necessary according to the provided memory
// budget. Chunks are queried sequentially, ensuring that the memory budget
// applies to only a single chunk at any given time.
//
// In addition to the memory budget, an "expected source count" must be provided
// to this function. This allows the query to better predict how many slabs
// it will encounter when querying a time span.
func (db *DB) QueryMemoryConstrained(
	ctx context.Context,
	query tspb.Query,
	queryResolution Resolution,
	sampleDuration, startNanos, endNanos, interpolationLimitNanos int64,
	resultAccount *mon.BoundAccount,
	workerMemMonitor *mon.BytesMonitor,
	workerMemBudget int64,
	expectedSourceCount int64,
) ([]tspb.TimeSeriesDatapoint, []string, error) {
	maxTimespan, err := getMaxTimespan(
		queryResolution, workerMemBudget, expectedSourceCount, interpolationLimitNanos,
	)
	if err != nil {
		return nil, nil, err
	}

	totalTimespan := endNanos - startNanos
	if maxTimespan >= totalTimespan {
		return db.Query(
			ctx,
			query,
			queryResolution,
			sampleDuration,
			startNanos,
			endNanos,
			interpolationLimitNanos,
			resultAccount,
			workerMemMonitor,
		)
	}

	allData := make([]tspb.TimeSeriesDatapoint, 0)
	allSourcesMap := make(map[string]struct{})
	for s, e := startNanos, startNanos+maxTimespan; s < endNanos; s, e = e, e+maxTimespan {
		// End span is not inclusive for partial queries.
		adjustedEnd := e - queryResolution.SampleDuration()
		// Do not exceed the specified endNanos.
		if adjustedEnd > endNanos {
			adjustedEnd = endNanos
		}
		data, sources, err := db.Query(
			ctx,
			query,
			queryResolution,
			sampleDuration,
			s,
			adjustedEnd,
			interpolationLimitNanos,
			resultAccount,
			workerMemMonitor,
		)
		if err != nil {
			return nil, nil, err
		}
		allData = append(allData, data...)
		for _, source := range sources {
			allSourcesMap[source] = struct{}{}
		}
	}

	allSources := make([]string, 0, len(allSourcesMap))
	for source := range allSourcesMap {
		allSources = append(allSources, source)
	}

	return allData, allSources, nil
}

// Query returns datapoints for the named time series during the supplied time
// span.  Data is returned as a series of consecutive data points.
//
// Raw data is queried only at the queryResolution supplied: if data for the
// named time series is not stored at the given resolution, an empty result will
// be returned.
//
// Raw data is converted into query results through a number of processing
// steps, which are executed in the following order:
//
// 1. Downsampling
// 2. Rate calculation (if requested)
// 3. Interpolation and Aggregation
//
// Raw data stored on the server is already downsampled into samples with
// interval length queryResolution.SampleDuration(); however, Result data can be
// further downsampled into a longer sample intervals based on a provided
// sampleDuration. sampleDuration must have a sample duration which is a
// positive integer multiple of the queryResolution's sample duration. The
// downsampling operation can compute a sum, total, max or min. Each downsampled
// datapoint's timestamp falls in the middle of the sample period it represents.
//
// After downsampling, values can be converted into a rate if requested by the
// query. Each data point's value is replaced by the derivative of the series at
// that timestamp, computed by comparing the datapoint to its predecessor. If a
// query requests a derivative, the returned value for each datapoint is
// expressed in units per second.
//
// If data for the named time series was collected from multiple sources, each
// returned datapoint will represent the sum of datapoints from all sources at
// the same time. The returned string slices contains a list of all sources for
// the metric which were aggregated to produce the result. In the case where one
// series is missing a data point that is present in other series, the missing
// data points for that series will be interpolated using linear interpolation.
func (db *DB) Query(
	ctx context.Context,
	query tspb.Query,
	queryResolution Resolution,
	sampleDuration, startNanos, endNanos, interpolationLimitNanos int64,
	resultAccount *mon.BoundAccount,
	workerMemMonitor *mon.BytesMonitor,
) ([]tspb.TimeSeriesDatapoint, []string, error) {
	resolutionSampleDuration := queryResolution.SampleDuration()
	// Verify that sampleDuration is a multiple of
	// queryResolution.SampleDuration().
	if sampleDuration < resolutionSampleDuration {
		return nil, nil, fmt.Errorf(
			"sampleDuration %d was not less that queryResolution.SampleDuration %d",
			sampleDuration,
			resolutionSampleDuration,
		)
	}
	if sampleDuration%resolutionSampleDuration != 0 {
		return nil, nil, fmt.Errorf(
			"sampleDuration %d is not a multiple of queryResolution.SampleDuration %d",
			sampleDuration,
			resolutionSampleDuration,
		)
	}

	// Create a local account to track memory usage local to this function.
	localAccount := workerMemMonitor.MakeBoundAccount()
	defer localAccount.Close(ctx)

	// Disallow queries in the future.
	systemTime := timeutil.Now().UnixNano()
	if startNanos > systemTime {
		return nil, nil, nil
	}
	if endNanos > systemTime {
		endNanos = systemTime
	}

	// Normalize startNanos to a sampleDuration boundary.
	startNanos -= startNanos % sampleDuration

	// If query is near the current moment and we are downsampling, normalize
	// endNanos to avoid querying an incomplete datapoint.
	if sampleDuration > resolutionSampleDuration &&
		endNanos > systemTime-resolutionSampleDuration {
		endNanos -= endNanos % sampleDuration
	}

	var rows []client.KeyValue
	if len(query.Sources) == 0 {
		// Based on the supplied timestamps and resolution, construct start and
		// end keys for a scan that will return every key with data relevant to
		// the query. Query slightly before and after the actual queried range
		// to allow interpolation of points at the start and end of the range.
		startKey := MakeDataKey(
			query.Name, "" /* source */, queryResolution, startNanos-interpolationLimitNanos,
		)
		endKey := MakeDataKey(
			query.Name, "" /* source */, queryResolution, endNanos+interpolationLimitNanos,
		).PrefixEnd()
		b := &client.Batch{}
		b.Scan(startKey, endKey)

		if err := db.db.Run(ctx, b); err != nil {
			return nil, nil, err
		}
		rows = b.Results[0].Rows
	} else {
		b := &client.Batch{}
		// Iterate over all key timestamps which may contain data for the given
		// sources, based on the given start/end time and the resolution.
		kd := queryResolution.SlabDuration()
		startKeyNanos := startNanos - interpolationLimitNanos
		startKeyNanos = startKeyNanos - (startKeyNanos % kd)
		endKeyNanos := endNanos + interpolationLimitNanos
		for currentTimestamp := startKeyNanos; currentTimestamp <= endKeyNanos; currentTimestamp += kd {
			for _, source := range query.Sources {
				key := MakeDataKey(query.Name, source, queryResolution, currentTimestamp)
				b.Get(key)
			}
		}
		err := db.db.Run(ctx, b)
		if err != nil {
			return nil, nil, err
		}
		for _, result := range b.Results {
			row := result.Rows[0]
			if row.Value == nil {
				continue
			}
			rows = append(rows, row)
		}
	}

	// Convert the queried source data into a set of data spans, one for each
	// source.
	sourceSpans, err := makeDataSpans(ctx, rows, startNanos, &localAccount)
	if err != nil {
		return nil, nil, err
	}

	// Choose an extractor function which will be used to return values from
	// each source for each sample period.
	extractor, err := getExtractionFunction(query.GetDownsampler())
	if err != nil {
		return nil, nil, err
	}

	// Choose downsampler function.
	downsampler, err := getDownsampleFunction(query.GetDownsampler())
	if err != nil {
		return nil, nil, err
	}

	// Create an interpolatingIterator for each dataSpan, adding each iterator
	// into a aggregatingIterator collection. This is also where we compute a
	// list of all sources with data present in the query.
	sources := make([]string, 0, len(sourceSpans))
	iters := make(aggregatingIterator, 0, len(sourceSpans))
	maxDistance := int32(interpolationLimitNanos / sampleDuration)
	for name, span := range sourceSpans {
		if err := resultAccount.Grow(ctx, int64(len(name))); err != nil {
			return nil, nil, err
		}
		sources = append(sources, name)
		iters = append(iters, newInterpolatingIterator(
			*span, 0, sampleDuration, maxDistance, extractor, downsampler, query.GetDerivative(),
		))
	}

	// Choose an aggregation function to use when taking values from the
	// aggregatingIterator.
	var aggFn func(aggregatingIterator) (float64, bool)
	switch query.GetSourceAggregator() {
	case tspb.TimeSeriesQueryAggregator_SUM:
		aggFn = aggregatingIterator.sum
	case tspb.TimeSeriesQueryAggregator_AVG:
		aggFn = aggregatingIterator.avg
	case tspb.TimeSeriesQueryAggregator_MAX:
		aggFn = aggregatingIterator.max
	case tspb.TimeSeriesQueryAggregator_MIN:
		aggFn = aggregatingIterator.min
	default:
		return nil, nil, fmt.Errorf(
			"query specified unknown time series aggregator: %s", query.GetSourceAggregator().String(),
		)
	}

	// Filter the result of the aggregation function through a leading edge
	// filter.
	cutoffNanos := timeutil.Now().UnixNano() - resolutionSampleDuration
	valueFn := iters.makeLeadingEdgeFilter(aggFn, cutoffNanos)

	// Iterate over all requested offsets, recording a value from the
	// aggregatingIterator at each offset encountered. If the query is
	// requesting a derivative, a rate of change is recorded instead of the
	// actual values.
	iters.init()
	if !iters.isValid() {
		// We have no data to return.
		return nil, sources, nil
	}

	var responseData []tspb.TimeSeriesDatapoint
	for iters.isValid() && iters.timestamp() <= endNanos {
		if value, valid := valueFn(); valid {
			if err := resultAccount.Grow(ctx, sizeOfDataPoint); err != nil {
				return nil, nil, err
			}
			response := tspb.TimeSeriesDatapoint{
				TimestampNanos: iters.timestamp(),
				Value:          value,
			}
			if query.GetDerivative() != tspb.TimeSeriesQueryDerivative_NONE {
				response.Value = response.Value / float64(sampleDuration) * float64(time.Second.Nanoseconds())
			}
			responseData = append(responseData, response)
		}
		iters.advance()
	}

	return responseData, sources, nil
}

// makeDataSpans constructs a new dataSpan for each distinct source encountered
// in the query. Each dataspan will contain all data queried from a single
// source.
func makeDataSpans(
	ctx context.Context, rows []client.KeyValue, startNanos int64, acc *mon.BoundAccount,
) (map[string]*dataSpan, error) {
	sourceSpans := make(map[string]*dataSpan)
	for _, row := range rows {
		var data roachpb.InternalTimeSeriesData
		if err := row.ValueProto(&data); err != nil {
			return nil, err
		}
		_, source, _, _, err := DecodeDataKey(row.Key)
		if err != nil {
			return nil, err
		}
		if _, ok := sourceSpans[source]; !ok {
			if err := acc.Grow(ctx, int64(len(source))+sizeOfDataSpan); err != nil {
				return nil, err
			}
			sourceSpans[source] = &dataSpan{
				startNanos:  startNanos,
				sampleNanos: data.SampleDurationNanos,
				datas:       make([]calibratedData, 0, 1),
			}
		}
		if err := acc.Grow(
			ctx, sizeOfSample*int64(len(data.Samples))+sizeOfCalibratedData,
		); err != nil {
			return nil, err
		}
		if err := sourceSpans[source].addData(data); err != nil {
			return nil, err
		}
	}

	return sourceSpans, nil
}

func getExtractionFunction(agg tspb.TimeSeriesQueryAggregator) (extractFn, error) {
	switch agg {
	case tspb.TimeSeriesQueryAggregator_AVG:
		return (roachpb.InternalTimeSeriesSample).Average, nil
	case tspb.TimeSeriesQueryAggregator_SUM:
		return (roachpb.InternalTimeSeriesSample).Summation, nil
	case tspb.TimeSeriesQueryAggregator_MAX:
		return (roachpb.InternalTimeSeriesSample).Maximum, nil
	case tspb.TimeSeriesQueryAggregator_MIN:
		return (roachpb.InternalTimeSeriesSample).Minimum, nil
	}
	return nil, errors.Errorf("query specified unknown time series downsampler %s", agg.String())
}

func downsampleSum(points ...roachpb.InternalTimeSeriesSample) float64 {
	result := 0.0
	for _, p := range points {
		result += p.Sum
	}
	return result
}

func downsampleMax(points ...roachpb.InternalTimeSeriesSample) float64 {
	result := points[0].Maximum()
	for _, p := range points[1:] {
		if p.Maximum() > result {
			result = p.Maximum()
		}
	}
	return result
}

func downsampleMin(points ...roachpb.InternalTimeSeriesSample) float64 {
	result := points[0].Minimum()
	for _, p := range points[1:] {
		if p.Minimum() > result {
			result = p.Minimum()
		}
	}
	return result
}

func downsampleAvg(points ...roachpb.InternalTimeSeriesSample) float64 {
	total := 0.0
	var count uint32
	for _, p := range points {
		total += p.Sum
		count += p.Count
	}
	return total / float64(count)
}

func getDownsampleFunction(agg tspb.TimeSeriesQueryAggregator) (downsampleFn, error) {
	switch agg {
	case tspb.TimeSeriesQueryAggregator_AVG:
		return downsampleAvg, nil
	case tspb.TimeSeriesQueryAggregator_SUM:
		return downsampleSum, nil
	case tspb.TimeSeriesQueryAggregator_MAX:
		return downsampleMax, nil
	case tspb.TimeSeriesQueryAggregator_MIN:
		return downsampleMin, nil
	}
	return nil, errors.Errorf("query specified unknown time series downsampler %s", agg.String())
}
