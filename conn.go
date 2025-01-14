package statsd

import (
	"io"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"time"
)

type WriteCloserWithTimeout interface {
	io.WriteCloser

	SetDeadline(time.Time) error
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

type conn struct {
	// Fields settable with options at Client's creation.
	addr          string
	errorHandler  func(error)
	timeout       time.Duration
	flushPeriod   time.Duration
	maxPacketSize int
	network       string
	tagFormat     TagFormat
	sendLastEndl  bool

	mu sync.Mutex
	// Fields guarded by the mutex.
	closed    bool
	w         WriteCloserWithTimeout
	buf       []byte
	rateCache map[float32]string
}

func newConn(conf connConfig, muted bool) (*conn, error) {
	c := &conn{
		addr:          conf.Addr,
		errorHandler:  conf.ErrorHandler,
		timeout:       conf.Timeout,
		flushPeriod:   conf.FlushPeriod,
		maxPacketSize: conf.MaxPacketSize,
		network:       conf.Network,
		tagFormat:     conf.TagFormat,
	}

	if c.network[:3] != "udp" {
		c.sendLastEndl = true
	}

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
				c.mu.Lock()
				if c.closed {
					ticker.Stop()
					c.mu.Unlock()
					return
				}
				c.flush(0)
				c.mu.Unlock()
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
			if c.timeout > 0 {
				c.w.SetDeadline(time.Now().Add(c.timeout))
			}
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

func (c *conn) metric(prefix, bucket string, n interface{}, typ string, rate float32, tags string) {
	c.mu.Lock()
	l := len(c.buf)
	c.appendBucket(prefix, bucket, tags)
	c.appendNumber(n)
	c.appendType(typ)
	c.appendRate(rate)
	c.closeMetric(tags)
	c.flushIfBufferFull(l)
	c.mu.Unlock()
}

func (c *conn) gauge(prefix, bucket string, value interface{}, tags string) {
	c.mu.Lock()
	l := len(c.buf)
	// To set a gauge to a negative value we must first set it to 0.
	// https://github.com/etsy/statsd/blob/master/docs/metric_types.md#gauges
	if isNegative(value) {
		c.appendBucket(prefix, bucket, tags)
		c.appendGauge(0, tags)
	}
	c.appendBucket(prefix, bucket, tags)
	c.appendGauge(value, tags)
	c.flushIfBufferFull(l)
	c.mu.Unlock()
}

func (c *conn) appendGauge(value interface{}, tags string) {
	c.appendNumber(value)
	c.appendType(GAUGE_S)
	c.closeMetric(tags)
}

func (c *conn) unique(prefix, bucket string, value string, tags string) {
	c.mu.Lock()
	l := len(c.buf)
	c.appendBucket(prefix, bucket, tags)
	c.appendString(value)
	c.appendType(SET_S)
	c.closeMetric(tags)
	c.flushIfBufferFull(l)
	c.mu.Unlock()
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

func (c *conn) flushIfBufferFull(lastSafeLen int) {
	if len(c.buf) > c.maxPacketSize {
		c.flush(lastSafeLen)
	}
}

// flush flushes the first n bytes of the buffer.
// If n is 0, the whole buffer is flushed.
func (c *conn) flush(n int) error {
	if len(c.buf) == 0 {
		return nil
	}
	if n == 0 {
		n = len(c.buf)
	}

	if c.w == nil {
		if err := c.dial(); err != nil {
			c.errorHandler(err)
			return err
		}
	}

	var err error
	if c.timeout > 0 {
		c.w.SetDeadline(time.Now().Add(c.timeout))
	}
	if c.sendLastEndl {
		// Don't trim the last \n, becouse persistent connection
		_, err = c.w.Write(c.buf[:n])
	} else {
		// Trim the last \n, StatsD does not like it.
		_, err = c.w.Write(c.buf[:n-1])
	}
	if err != nil {
		c.handleError(err)
		c.w.Close()
		c.w = nil
	}
	if n < len(c.buf) {
		copy(c.buf, c.buf[n:])
	}
	c.buf = c.buf[:len(c.buf)-n]

	return err
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
