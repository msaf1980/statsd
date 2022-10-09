# statsd

## Introduction

statsd is a simple and efficient [Statsd](https://github.com/etsy/statsd)
client. based on `https://godoc.org/gopkg.in/alexcesaro`.

See the [benchmark](https://github.com/msaf1980/statsdbench) for a comparison
with other Go StatsD clients.

## Features

- Supports all StatsD metrics: counter, gauge, timing and set
- Supports InfluxDB and Datadog tags
- Fast and GC-friendly: all functions for sending metrics do not allocate
- Efficient: metrics are buffered by default
- Simple and clean API
- 100% test coverage
- Versioned API using gopkg.in


## Documentation

https://godoc.org/gopkg.in/msaf1980/statsd.v2


## Download

    go get gopkg.in/msaf1980/statsd.v2


## Example

See the [examples in the documentation](https://godoc.org/gopkg.in/msaf1980/statsd.v2#example-package).


## License

[MIT](LICENSE)
