package metrics

// movingAvg implements a moving avg over the last lookbackAvg values.
// The precision is limited to two decimal places. The values added need to be
// non-negative and in the range of 0.00 to 655.35.
// Currently, this struct is used to track CPU usage where each sample is the
// aggregate CPU usage floating point number that varies from zero to the max
// number of cores.
// The CPU usage is strictly positive so the buckets type is set to uint16
// If needed it can be changed to a more general purpose struct, making the
// bucket type and the totalSum generic.
type movingAvg struct {
	buckets       []uint16
	slidingWindow int
	currentItem   int
	// totalSum is a larger int type (instead of uint16) so it can hold the
	// sum of large number of samples.
	totalSum    int
	sampleCount int
}

// newMovingAvg constructs a moving Avg.
// slidingWindow sets over how many samples it would do the average.
func newMovingAvg(slidingWindow int) *movingAvg {
	return &movingAvg{
		buckets:       make([]uint16, slidingWindow),
		slidingWindow: slidingWindow,
	}
}

// add a value to the moving average.
func (ma *movingAvg) add(val float64) {
	ma.totalSum -= int(ma.buckets[ma.currentItem])
	// This restricts the precision of what we are storing to two decimal places.
	ma.totalSum += int(val * 100)
	ma.buckets[ma.currentItem] = uint16(val * 100)
	ma.currentItem++
	if ma.currentItem >= ma.slidingWindow {
		ma.currentItem = 0
	}
	if ma.sampleCount < ma.slidingWindow {
		ma.sampleCount++
	}
}

// fillRatio returns sample count divided by sliding window size.
func (ma *movingAvg) fillRatio() float64 {
	return float64(ma.sampleCount) / float64(ma.slidingWindow)
}

// avg returns the avg value over the duration look back window interval.
func (ma *movingAvg) avg() float64 {
	return float64(ma.totalSum/ma.slidingWindow) / 100
}
