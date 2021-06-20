package statsd

import (
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"strconv"
	"time"

	lockfree_queue "github.com/msaf1980/go-lockfree-queue"
)

type conn struct {
	// Fields settable with options at Client's creation.
	addr          string
	errorHandler  func(error)
	timeout       time.Duration
	flushPeriod   time.Duration
	maxPacketSize int
	maxQueued     int
	network       string
	tagFormat     TagFormat

	q *lockfree_queue.Queue
	// Fields guarded by the mutex.
	closed    bool
	w         io.WriteCloser
	buf       []byte
	rateCache map[float32]string
}

func newConn(conf connConfig, muted bool) (*conn, error) {
	c := &conn{
		addr:          conf.Addr,
		errorHandler:  conf.ErrorHandler,
		timeout:       5 * time.Second,
		flushPeriod:   conf.FlushPeriod,
		maxPacketSize: conf.MaxPacketSize,
		maxQueued:     1024,
		network:       conf.Network,
		tagFormat:     conf.TagFormat,
	}

	c.q = lockfree_queue.NewQueue(c.maxQueued)

	if muted {
		return c, nil
	}

	var err error

	err = c.dial()
	c.handleError(err)

	// To prevent a buffer overflow add some capacity to the buffer to allow for
	// an additional metric.
	c.buf = make([]byte, 0, c.maxPacketSize+200)

	if c.flushPeriod > 0 {
		go func() {
			ticker := time.NewTicker(c.flushPeriod)
			for _ = range ticker.C {
				c.flush(0)
				if c.closed {
					if c.w != nil {
						err := c.w.Close()
						c.errorHandler(err)
					}
					ticker.Stop()
					return
				}
			}
		}()
	}

	return c, err
}

func (c *conn) dial() error {
	var err error
	c.w, err = dialTimeout(c.network, c.addr, c.timeout)
	if err != nil {
		return err
	}
	// When using UDP do a quick check to see if something is listening on the
	// given port to return an error as soon as possible.
	if c.network[:3] == "udp" {
		for i := 0; i < 2; i++ {
			_, err = c.w.Write(nil)
			if err != nil {
				_ = c.w.Close()
				c.w = nil
				return err
			}
		}
	}
	return nil
}

func (c *conn) Put(m *Metric) error {
	if !c.q.Put(m) {
		// drop last two elements and try put again
		c.q.Get()
		c.q.Get()
		c.q.Put(m)
	}
	return nil
}

func (c *conn) Get() (bool, error) {
	metric, _ := c.q.Get()
	if metric != nil {
		l := len(c.buf)
		m := metric.(*Metric)
		switch m.Type {
		case COUNT:
			c.metric(m.Prefix, m.Bucket, m.Value, COUNT_S, m.Rate, m.Tags)
		case GAUGE:
			c.gauge(m.Prefix, m.Bucket, m.Value, m.Tags)
		case TIMINGS:
			c.metric(m.Prefix, m.Bucket, m.Value, TIMINGS_S, m.Rate, m.Tags)
		case HISTOGRAM:
			c.metric(m.Prefix, m.Bucket, m.Value, HISTOGRAM_S, m.Rate, m.Tags)
		default:
			c.errorHandler(fmt.Errorf("unknown metric type: %d", m.Type))
		}
		m.Reset() /* put back in metricPool */
		return true, c.flushIfBufferFull(l)
	}
	return false, nil
}

func (c *conn) getNoFlush() bool {
	metric, _ := c.q.Get()
	if metric != nil {
		m := metric.(*Metric)
		switch m.Type {
		case COUNT:
			c.metric(m.Prefix, m.Bucket, m.Value, COUNT_S, m.Rate, m.Tags)
		case GAUGE:
			c.gauge(m.Prefix, m.Bucket, m.Value, m.Tags)
		case TIMINGS:
			c.metric(m.Prefix, m.Bucket, m.Value, TIMINGS_S, m.Rate, m.Tags)
		case HISTOGRAM:
			c.metric(m.Prefix, m.Bucket, m.Value, HISTOGRAM_S, m.Rate, m.Tags)
		}
		m.Reset() /* put back in metricPool */
		return true
	}
	return false
}

func (c *conn) metric(prefix, bucket string, n interface{}, typ string, rate float32, tags string) {
	c.appendBucket(prefix, bucket, tags)
	c.appendNumber(n)
	c.appendType(typ)
	c.appendRate(rate)
	c.closeMetric(tags)
}

func (c *conn) gauge(prefix, bucket string, value interface{}, tags string) {
	// To set a gauge to a negative value we must first set it to 0.
	// https://github.com/etsy/statsd/blob/master/docs/metric_types.md#gauges
	if isNegative(value) {
		c.appendBucket(prefix, bucket, tags)
		c.appendGauge(0, tags)
	}
	c.appendBucket(prefix, bucket, tags)
	c.appendGauge(value, tags)
}

func (c *conn) appendGauge(value interface{}, tags string) {
	c.appendNumber(value)
	c.appendType(GAUGE_S)
	c.closeMetric(tags)
}

func (c *conn) unique(prefix, bucket string, value string, tags string) {
	c.appendBucket(prefix, bucket, tags)
	c.appendString(value)
	c.appendType(SET_S)
	c.closeMetric(tags)
}

func (c *conn) appendByte(b byte) {
	c.buf = append(c.buf, b)
}

func (c *conn) appendString(s string) {
	c.buf = append(c.buf, s...)
}

func (c *conn) appendNumber(v interface{}) {
	switch n := v.(type) {
	case int:
		c.buf = strconv.AppendInt(c.buf, int64(n), 10)
	case uint:
		c.buf = strconv.AppendUint(c.buf, uint64(n), 10)
	case int64:
		c.buf = strconv.AppendInt(c.buf, n, 10)
	case uint64:
		c.buf = strconv.AppendUint(c.buf, n, 10)
	case int32:
		c.buf = strconv.AppendInt(c.buf, int64(n), 10)
	case uint32:
		c.buf = strconv.AppendUint(c.buf, uint64(n), 10)
	case int16:
		c.buf = strconv.AppendInt(c.buf, int64(n), 10)
	case uint16:
		c.buf = strconv.AppendUint(c.buf, uint64(n), 10)
	case int8:
		c.buf = strconv.AppendInt(c.buf, int64(n), 10)
	case uint8:
		c.buf = strconv.AppendUint(c.buf, uint64(n), 10)
	case float64:
		c.buf = strconv.AppendFloat(c.buf, n, 'f', -1, 64)
	case float32:
		c.buf = strconv.AppendFloat(c.buf, float64(n), 'f', -1, 32)
	}
}

func isNegative(v interface{}) bool {
	switch n := v.(type) {
	case int:
		return n < 0
	case uint:
		return n < 0
	case int64:
		return n < 0
	case uint64:
		return n < 0
	case int32:
		return n < 0
	case uint32:
		return n < 0
	case int16:
		return n < 0
	case uint16:
		return n < 0
	case int8:
		return n < 0
	case uint8:
		return n < 0
	case float64:
		return n < 0
	case float32:
		return n < 0
	}
	return false
}

func (c *conn) appendBucket(prefix, bucket string, tags string) {
	c.appendString(prefix)
	c.appendString(bucket)
	if c.tagFormat == InfluxDB {
		c.appendString(tags)
	}
	c.appendByte(':')
}

func (c *conn) appendType(t string) {
	c.appendString(t)
}

func (c *conn) appendRate(rate float32) {
	if rate == 1 {
		return
	}
	if c.rateCache == nil {
		c.rateCache = make(map[float32]string)
	}

	c.appendString("|@")
	if s, ok := c.rateCache[rate]; ok {
		c.appendString(s)
	} else {
		s = strconv.FormatFloat(float64(rate), 'f', -1, 32)
		c.rateCache[rate] = s
		c.appendString(s)
	}
}

func (c *conn) closeMetric(tags string) {
	if c.tagFormat == Datadog {
		c.appendString(tags)
	}
	c.appendByte('\n')
}

func (c *conn) reset(n int) {
	c.buf = c.buf[0:n]
}

func (c *conn) flushBuffer(n int) error {
	// Trim the last \n, StatsD does not like it.
	if n == 0 || n > len(c.buf) {
		n = len(c.buf)
	}
	if n == 0 {
		return nil
	}
	var err error
	if c.w != nil {
		_, err = c.w.Write(c.buf[:n-1])
		c.handleError(err)
	}
	if n < len(c.buf) {
		copy(c.buf, c.buf[n:])
	}
	c.reset(len(c.buf) - n)
	return err
}

func (c *conn) flushIfBufferFull(lastSafeLen int) error {
	if len(c.buf) > c.maxPacketSize {
		return c.flushBuffer(lastSafeLen)
	}
	return nil
}

// flush flushes the first n metrics from the queue.
// If n is 0, the whole buffer is flushed.
func (c *conn) flush(n int) {
	if c.q.Size() == 0 {
		return
	}
	if n == 0 {
		n = math.MaxInt32
	}

	if c.w == nil {
		if err := c.dial(); err != nil {
			c.errorHandler(err)
			return
		}
	}

	for n > 0 {
		ok, err := c.Get()
		c.errorHandler(err)
		if err != nil {
			return
		} else if !ok {
			break
		}
		n--
	}
	c.flushBuffer(0)
}

func (c *conn) handleError(err error) {
	if err != nil && c.errorHandler != nil {
		c.errorHandler(err)
	}
}

// Stubbed out for testing.
var (
	dialTimeout = net.DialTimeout
	now         = time.Now
	randFloat   = rand.Float32
)
