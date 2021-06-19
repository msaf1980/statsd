package statsd

type Type uint8

const (
	COUNT Type = iota
	GAUGE
	TIMING
	HISTOGRAM
)

type Metric struct {
	Type   Type
	Bucket string
	Value  interface{}
}
