package statsd

import "time"

// A Client represents a StatsD client.
type Client struct {
	conn   *conn
	muted  bool
	rate   float32
	prefix string
	tags   string
}

// New returns a new Client  (error is connection error and might be temporary)
func New(opts ...Option) (*Client, error) {
	// The default configuration.
	conf := &config{
		Client: clientConfig{
			Rate: 1,
		},
		Conn: connConfig{
			Addr:        ":8125",
			FlushPeriod: 100 * time.Millisecond,
			// Worst-case scenario:
			// Ethernet MTU - IPv6 Header - TCP Header = 1500 - 40 - 20 = 1440
			MaxPacketSize: 1440,
			Network:       "udp",
		},
	}
	for _, o := range opts {
		o(conf)
	}

	conn, err := newConn(conf.Conn, conf.Client.Muted)
	c := &Client{
		conn:  conn,
		muted: conf.Client.Muted,
	}
	c.rate = conf.Client.Rate
	c.prefix = conf.Client.Prefix
	c.tags = joinTags(conf.Conn.TagFormat, conf.Client.Tags)
	return c, err
}

// Clone returns a clone of the Client. The cloned Client inherits its
// configuration from its parent.
//
// All cloned Clients share the same connection, so cloning a Client is a cheap
// operation.
func (c *Client) Clone(opts ...Option) *Client {
	tf := c.conn.tagFormat
	conf := &config{
		Client: clientConfig{
			Rate:   c.rate,
			Prefix: c.prefix,
			Tags:   splitTags(tf, c.tags),
		},
	}
	for _, o := range opts {
		o(conf)
	}

	clone := &Client{
		conn:   c.conn,
		muted:  c.muted || conf.Client.Muted,
		rate:   conf.Client.Rate,
		prefix: conf.Client.Prefix,
		tags:   joinTags(tf, conf.Client.Tags),
	}
	clone.conn = c.conn
	return clone
}

// Count adds n to bucket.
func (c *Client) Count(bucket string, value interface{}) error {
	if c.skip() {
		return nil
	}
	//c.conn.metric(c.prefix, bucket, n, COUNT_S, c.rate, c.tags)
	m := metricPool.New().(*Metric)
	m.Type = COUNT
	m.Prefix = c.prefix
	m.Bucket = bucket
	m.Tags = c.tags
	m.Value = value
	m.Rate = c.rate
	return c.conn.Put(m)
}

func (c *Client) skip() bool {
	return c.muted || (c.rate != 1 && randFloat() > c.rate)
}

// Increment increment the given bucket. It is equivalent to Count(bucket, 1).
func (c *Client) Increment(bucket string) error {
	return c.Count(bucket, 1)
}

// Gauge records an absolute value for the given bucket.
func (c *Client) Gauge(bucket string, value interface{}) error {
	if c.skip() {
		return nil
	}
	//c.conn.gauge(c.prefix, bucket, value, c.tags)
	m := metricPool.New().(*Metric)
	m.Type = GAUGE
	m.Prefix = c.prefix
	m.Bucket = bucket
	m.Tags = c.tags
	m.Value = value
	return c.conn.Put(m)
}

// Timing sends a timing value to a bucket.
func (c *Client) Timing(bucket string, value interface{}) error {
	if c.skip() {
		return nil
	}
	//c.conn.metric(c.prefix, bucket, value, TIMINGS_S, c.rate, c.tags)
	m := metricPool.New().(*Metric)
	m.Type = TIMINGS
	m.Prefix = c.prefix
	m.Bucket = bucket
	m.Tags = c.tags
	m.Value = value
	m.Rate = c.rate
	return c.conn.Put(m)
}

// Histogram sends an histogram value to a bucket.
func (c *Client) Histogram(bucket string, value interface{}) error {
	if c.skip() {
		return nil
	}
	//c.conn.metric(c.prefix, bucket, value, HISTOGRAM_S, c.rate, c.tags)
	m := metricPool.New().(*Metric)
	m.Type = HISTOGRAM
	m.Prefix = c.prefix
	m.Bucket = bucket
	m.Tags = c.tags
	m.Value = value
	m.Rate = c.rate
	return c.conn.Put(m)
}

// A Timing is an helper object that eases sending timing values.
type Timing struct {
	start time.Time
	c     *Client
}

// NewTiming creates a new Timing.
func (c *Client) NewTiming() Timing {
	return Timing{start: now(), c: c}
}

// Send sends the time elapsed since the creation of the Timing.
func (t Timing) Send(bucket string) {
	t.c.Timing(bucket, int(t.Duration()/time.Millisecond))
}

// Duration returns the time elapsed since the creation of the Timing.
func (t Timing) Duration() time.Duration {
	return now().Sub(t.start)
}

// Unique sends the given value to a set bucket.
func (c *Client) Unique(bucket string, value string) {
	if c.skip() {
		return
	}
	c.conn.unique(c.prefix, bucket, value, c.tags)
}

// flush flushes the closed Client's buffer.
func (c *Client) flush() {
	if !c.muted || c.conn.closed {
		c.conn.flush(0)
	}
}

// Close flushes the Client's buffer and releases the associated ressources. The
// Client and all the cloned Clients must not be used afterward.
func (c *Client) Close() {
	if c.muted {
		return
	}
	c.conn.closed = true
}
