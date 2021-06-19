package statsd

type Type uint8

const (
	COUNT Type = iota
	GAUGE
	TIMINGS
	HISTOGRAM
)

var (
	COUNT_S     = "|c"
	GAUGE_S     = "|g"
	TIMINGS_S   = "|ms"
	HISTOGRAM_S = "|h"
	SET_S       = "|s"
)

type Metric struct {
	Type   Type
	Bucket string
	Prefix string
	Tags   string
	Value  interface{}
	Rate   float32
}
