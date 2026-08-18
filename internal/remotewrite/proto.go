package remotewrite

import (
	"encoding/binary"
	"math"
	"sort"
)

// The Prometheus remote write payload is a snappy-compressed protobuf of:
//
//	message WriteRequest { repeated TimeSeries timeseries = 1; }
//	message TimeSeries   { repeated Label labels = 1; repeated Sample samples = 2; }
//	message Label        { string name = 1; string value = 2; }
//	message Sample       { double value = 1; int64 timestamp = 2; }
//
// That is small enough to encode by hand, which keeps protoc and the generated
// Prometheus packages out of the build entirely.

type Label struct {
	Name  string
	Value string
}

type Series struct {
	Labels []Label
	Value  float64
	// TimestampMS is Unix milliseconds, as the protocol requires.
	TimestampMS int64
}

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// appendTagged writes a length-delimited field: its tag, its length, its bytes.
func appendTagged(b []byte, field int, payload []byte) []byte {
	b = appendVarint(b, uint64(field)<<3|2)
	b = appendVarint(b, uint64(len(payload)))
	return append(b, payload...)
}

func appendString(b []byte, field int, s string) []byte {
	b = appendVarint(b, uint64(field)<<3|2)
	b = appendVarint(b, uint64(len(s)))
	return append(b, s...)
}

func encodeLabel(l Label) []byte {
	var b []byte
	b = appendString(b, 1, l.Name)
	b = appendString(b, 2, l.Value)
	return b
}

func encodeSample(value float64, tsMS int64) []byte {
	var b []byte
	// value: field 1, wire type 1 (fixed64 little-endian double)
	b = appendVarint(b, 1<<3|1)
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], math.Float64bits(value))
	b = append(b, buf[:]...)
	// timestamp: field 2, wire type 0 (varint), zig-zag free since it is positive
	b = appendVarint(b, 2<<3|0)
	b = appendVarint(b, uint64(tsMS))
	return b
}

func encodeSeries(s Series) []byte {
	// Remote write requires label pairs sorted by name; Mimir rejects the
	// request outright if they are not.
	labels := make([]Label, len(s.Labels))
	copy(labels, s.Labels)
	sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })

	var b []byte
	for _, l := range labels {
		b = appendTagged(b, 1, encodeLabel(l))
	}
	b = appendTagged(b, 2, encodeSample(s.Value, s.TimestampMS))
	return b
}

// EncodeWriteRequest marshals a batch of series into an uncompressed WriteRequest.
func EncodeWriteRequest(series []Series) []byte {
	var b []byte
	for _, s := range series {
		b = appendTagged(b, 1, encodeSeries(s))
	}
	return b
}
