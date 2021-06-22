package statsd

import (
	"io"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type WriteCloserWithTimeout interface {
	io.WriteCloser

	SetDeadline(time.Time) error
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// locked state for lockedBuf
const (
	BUF_UNLOCKED   int32 = 0  // Can be locked (for write or flush)
	BUF_LOCK_WRITE int32 = 1  // Locked for write
	BUF_NEED_FLUSH int32 = -1 // Need flush and can't be locked for write
)

type lockedBuf struct {
	locked int32
	data   []byte
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
	isStream      bool

	mu sync.Mutex
	// Fields guarded by the mutex.
	closed    bool
	closeChan chan bool
	flushChan chan int32
	w         WriteCloserWithTimeout
	buf       [2]lockedBuf
	activeBuf int32 // active buffer
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
		c.isStream = true
	}

	if muted {
		return c, nil
	}

	c.rateCache = make(map[float32]string)

	// To prevent a buffer overflow add some capacity to the buffer to allow for
	// an additional metric.
	for i := 0; i < len(c.buf); i++ {
		c.buf[i].data = make([]byte, 0, c.maxPacketSize+200)
	}

	var err error

	err = c.dial()
	c.handleError(err)

	if c.flushPeriod > 0 {
		c.closeChan = make(chan bool)
		c.flushChan = make(chan int32, 2)
		go func() {
			ticker := time.NewTicker(c.flushPeriod)
			for {
				select {
				case <-ticker.C:
					if !c.closed {
						bufNum := c.switchBufUnlocked()
						c.flush(0, bufNum)
					}
				case bufNum := <-c.flushChan:
					if c.bufNeedFlush(bufNum) {
						c.flush(0, bufNum)
					}
				case <-c.closeChan:
					c.closed = true
					break
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
	bufNum := c.lockBuf()
	l := len(c.buf[bufNum].data)
	c.buf[bufNum].appendBucket(prefix, bucket, tags, c.tagFormat)
	c.buf[bufNum].appendNumber(n)
	c.buf[bufNum].appendType(typ)
	c.buf[bufNum].appendRate(rate, c.rateCache)
	c.buf[bufNum].closeMetric(tags, c.tagFormat)
	c.flushIfBufferFull(l, bufNum)
}

func (c *conn) gauge(prefix, bucket string, value interface{}, tags string) {
	bufNum := c.lockBuf()
	l := len(c.buf[bufNum].data)
	// To set a gauge to a negative value we must first set it to 0.
	// https://github.com/etsy/statsd/blob/master/docs/metric_types.md#gauges
	if isNegative(value) {
		c.buf[bufNum].appendBucket(prefix, bucket, tags, c.tagFormat)
		c.buf[bufNum].appendGauge(0, tags, c.tagFormat)
	}
	c.buf[bufNum].appendBucket(prefix, bucket, tags, c.tagFormat)
	c.buf[bufNum].appendGauge(value, tags, c.tagFormat)
	c.flushIfBufferFull(l, bufNum)
}

func (c *conn) unique(prefix, bucket string, value string, tags string) {
	bufNum := c.lockBuf()
	l := len(c.buf[bufNum].data)
	c.buf[bufNum].appendBucket(prefix, bucket, tags, c.tagFormat)
	c.buf[bufNum].appendString(value)
	c.buf[bufNum].appendType(SET_S)
	c.buf[bufNum].closeMetric(tags, c.tagFormat)
	c.flushIfBufferFull(l, bufNum)
}

func (c *lockedBuf) appendGauge(value interface{}, tags string, tagFormat TagFormat) {
	c.appendNumber(value)
	c.appendType(GAUGE_S)
	c.closeMetric(tags, tagFormat)
}

func (c *lockedBuf) appendByte(b byte) {
	c.data = append(c.data, b)
}

func (c *lockedBuf) appendString(s string) {
	c.data = append(c.data, s...)
}

func (c *lockedBuf) appendNumber(v interface{}) {
	switch n := v.(type) {
	case int:
		c.data = strconv.AppendInt(c.data, int64(n), 10)
	case uint:
		c.data = strconv.AppendUint(c.data, uint64(n), 10)
	case int64:
		c.data = strconv.AppendInt(c.data, n, 10)
	case uint64:
		c.data = strconv.AppendUint(c.data, n, 10)
	case int32:
		c.data = strconv.AppendInt(c.data, int64(n), 10)
	case uint32:
		c.data = strconv.AppendUint(c.data, uint64(n), 10)
	case int16:
		c.data = strconv.AppendInt(c.data, int64(n), 10)
	case uint16:
		c.data = strconv.AppendUint(c.data, uint64(n), 10)
	case int8:
		c.data = strconv.AppendInt(c.data, int64(n), 10)
	case uint8:
		c.data = strconv.AppendUint(c.data, uint64(n), 10)
	case float64:
		c.data = strconv.AppendFloat(c.data, n, 'f', -1, 64)
	case float32:
		c.data = strconv.AppendFloat(c.data, float64(n), 'f', -1, 32)
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

func (c *lockedBuf) appendBucket(prefix, bucket string, tags string, tagFormat TagFormat) {
	c.appendString(prefix)
	c.appendString(bucket)
	if tagFormat == InfluxDB {
		c.appendString(tags)
	}
	c.appendByte(':')
}

func (c *lockedBuf) appendType(t string) {
	c.appendString(t)
}

func (c *lockedBuf) appendRate(rate float32, rateCache map[float32]string) {
	if rate == 1 {
		return
	}

	c.appendString("|@")
	if s, ok := rateCache[rate]; ok {
		c.appendString(s)
	} else {
		s = strconv.FormatFloat(float64(rate), 'f', -1, 32)
		rateCache[rate] = s
		c.appendString(s)
	}
}

func (c *lockedBuf) closeMetric(tags string, tagFormat TagFormat) {
	if tagFormat == Datadog {
		c.appendString(tags)
	}
	c.appendByte('\n')
}

// lockBuf lock unlocked bufer for write
func (c *conn) lockBuf() int32 {
	for {
		bufNum := atomic.LoadInt32(&c.activeBuf)
		if atomic.CompareAndSwapInt32(&c.buf[bufNum].locked, BUF_UNLOCKED, BUF_LOCK_WRITE) {
			return bufNum
		}
	}
}

// releaseBuf unlock locked bufer (for write)
func (c *conn) releaseBuf(bufNum int32) bool {
	return atomic.CompareAndSwapInt32(&c.buf[bufNum].locked, BUF_LOCK_WRITE, BUF_UNLOCKED)
}

// switchBufUnlocked for run in background flusher
func (c *conn) switchBufUnlocked() int32 {
	for {
		bufNum := atomic.LoadInt32(&c.activeBuf)
		if atomic.CompareAndSwapInt32(&c.buf[bufNum].locked, BUF_UNLOCKED, BUF_NEED_FLUSH) {
			newBufNum := bufNum + 1
			if int(newBufNum) == len(c.buf) {
				newBufNum = 0
			}
			atomic.CompareAndSwapInt32(&c.activeBuf, bufNum, newBufNum)
			return bufNum
		}
	}
}

// switchBufNum call in writer only
func (c *conn) switchBufNum(bufNum int32) int32 {
	if atomic.CompareAndSwapInt32(&c.buf[bufNum].locked, BUF_LOCK_WRITE, BUF_NEED_FLUSH) {
		newBufNum := bufNum + 1
		if int(newBufNum) == len(c.buf) {
			newBufNum = 0
		}
		if atomic.CompareAndSwapInt32(&c.activeBuf, bufNum, newBufNum) {
			return newBufNum
		} else {
			return -2
		}
	}
	return -1
}

// func (c *conn) lockBufFlush() int32 {
// 	for {
// 		bufNum := atomic.LoadInt32(&c.activeBuf)
// 		if atomic.CompareAndSwapInt32(&c.buf[bufNum].locked, BUF_UNLOCKED, BUF_NEED_FLUSH) {
// 			return bufNum
// 		}
// 	}
// }

// bufNeedFlush check if buffer need to be flushed
func (c *conn) bufNeedFlush(bufNum int32) bool {
	return atomic.LoadInt32(&c.buf[bufNum].locked) == BUF_NEED_FLUSH
}

// releaseBufFlush release buffer after flush completed
func (c *conn) releaseBufFlush(bufNum int32, n int) bool {
	if len(c.buf[bufNum].data) > n {
		c.buf[bufNum].data = c.buf[bufNum].data[:n]
	}
	return atomic.CompareAndSwapInt32(&c.buf[bufNum].locked, BUF_NEED_FLUSH, BUF_UNLOCKED)
}

func (c *conn) flushIfBufferFull(lastSafeLen int, bufNum int32) error {
	if len(c.buf[bufNum].data) >= c.maxPacketSize {
		c.switchBufNum(bufNum)
		if c.flushPeriod > 0 {
			c.flushChan <- bufNum
			return nil
		} else {
			return c.flush(lastSafeLen, bufNum)
		}
	} else {
		if !c.releaseBuf(bufNum) {
			panic("release buffer failed")
		}
	}

	return nil
}

// flush flushes the first n bytes of the buffer.
// If n is 0, the whole buffer is flushed.
func (c *conn) flush(n int, bufNum int32) error {
	if len(c.buf[bufNum].data) == 0 {
		c.releaseBufFlush(bufNum, 0)
		return nil
	}
	if n == 0 {
		n = len(c.buf[bufNum].data)
	}

	if c.w == nil {
		if err := c.dial(); err != nil {
			c.releaseBufFlush(bufNum, 0) // clear buffer for prevent overflow
			c.errorHandler(err)
			return err
		}
	}

	var err error
	if c.timeout > 0 {
		c.w.SetDeadline(time.Now().Add(c.timeout))
	}
	if c.isStream {
		// Don't trim the last \n, not a datagram
		_, err = c.w.Write(c.buf[bufNum].data[:n])
		if err != nil {
			if err = c.dial(); err != nil {
				c.releaseBufFlush(bufNum, 0) // clear buffer for prevent overflow
				c.errorHandler(err)
				return err
			}
			if c.timeout > 0 {
				c.w.SetDeadline(time.Now().Add(c.timeout))
			}
			_, err = c.w.Write(c.buf[bufNum].data[:n])
		}
	} else {
		// Trim the last \n, StatsD does not like it.
		_, err = c.w.Write(c.buf[bufNum].data[:n-1])
	}
	if err != nil {
		c.w.Close()
		c.w = nil
	}
	tailLen := len(c.buf[bufNum].data) - n
	if tailLen > 0 {
		copy(c.buf[bufNum].data, c.buf[bufNum].data[n:])
	}
	c.releaseBufFlush(bufNum, tailLen)

	c.handleError(err)

	return err
}

func (c *conn) handleError(err error) {
	if err != nil && c.errorHandler != nil {
		c.errorHandler(err)
	}
}

// Flush flushes the active Connect's buffer.
func (c *conn) Flush() error {
	bufNum := c.switchBufUnlocked()
	if c.flushPeriod == 0 {
		err := c.flush(0, bufNum)
		if err != nil {
			c.handleError(err)
		}

		return err
	} else {
		c.flushChan <- bufNum
		return nil
	}
}

// Close flushes the Connection's buffers and releases the associated ressources.
func (c *conn) Close() error {
	var err error
	if c.flushPeriod > 0 {
		c.closeChan <- true
	} else {
		c.closed = true
	}
	if c.w == nil {
		err = c.dial()
		if err != nil {
			c.handleError(err)
			c.closed = true
			return err
		}
	}
	for i := 0; i < 2*len(c.buf); i++ { // Loop 2 for flush if buffer size > MaxPacketSize
		bufNum := c.switchBufUnlocked()
		err = c.flush(0, bufNum)
		if err != nil {
			c.handleError(err)
			break
		}
	}
	if c.w != nil {
		err = c.w.Close()
		c.handleError(err)
	}

	return err
}

// Stubbed out for testing.
var (
	dialTimeout = net.DialTimeout
	now         = time.Now
	randFloat   = rand.Float32
)
