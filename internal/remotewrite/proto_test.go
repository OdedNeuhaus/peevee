package remotewrite

import (
	"encoding/binary"
	"math"
	"testing"
)

// decodeWriteRequest is an independent minimal protobuf reader used only by the
// tests, so the encoder is checked against a separate implementation rather
// than against itself.
type decodedSeries struct {
	labels map[string]string
	value  float64
	ts     int64
}

func readVarint(b []byte, i int) (uint64, int) {
	var v uint64
	var shift uint
	for {
		c := b[i]
		i++
		v |= uint64(c&0x7f) << shift
		if c < 0x80 {
			return v, i
		}
		shift += 7
	}
}

func decodeWriteRequest(t *testing.T, b []byte) []decodedSeries {
	t.Helper()
	var out []decodedSeries
	for i := 0; i < len(b); {
		key, ni := readVarint(b, i)
		i = ni
		if key != 1<<3|2 {
			t.Fatalf("unexpected top-level field key %d", key)
		}
		n, ni := readVarint(b, i)
		i = ni
		out = append(out, decodeSeries(t, b[i:i+int(n)]))
		i += int(n)
	}
	return out
}

func decodeSeries(t *testing.T, b []byte) decodedSeries {
	t.Helper()
	ds := decodedSeries{labels: map[string]string{}}
	var order []string
	for i := 0; i < len(b); {
		key, ni := readVarint(b, i)
		i = ni
		n, ni := readVarint(b, i)
		i = ni
		payload := b[i : i+int(n)]
		i += int(n)

		switch key >> 3 {
		case 1: // Label
			name, value := decodeLabel(t, payload)
			ds.labels[name] = value
			order = append(order, name)
		case 2: // Sample
			ds.value, ds.ts = decodeSample(t, payload)
		default:
			t.Fatalf("unexpected TimeSeries field %d", key>>3)
		}
	}
	for j := 1; j < len(order); j++ {
		if order[j-1] > order[j] {
			t.Errorf("labels are not sorted by name: %q came before %q", order[j-1], order[j])
		}
	}
	return ds
}

func decodeLabel(t *testing.T, b []byte) (string, string) {
	t.Helper()
	var name, value string
	for i := 0; i < len(b); {
		key, ni := readVarint(b, i)
		i = ni
		n, ni := readVarint(b, i)
		i = ni
		s := string(b[i : i+int(n)])
		i += int(n)
		switch key >> 3 {
		case 1:
			name = s
		case 2:
			value = s
		}
	}
	return name, value
}

func decodeSample(t *testing.T, b []byte) (float64, int64) {
	t.Helper()
	var value float64
	var ts int64
	for i := 0; i < len(b); {
		key, ni := readVarint(b, i)
		i = ni
		switch key {
		case 1<<3 | 1: // fixed64 double
			value = math.Float64frombits(binary.LittleEndian.Uint64(b[i : i+8]))
			i += 8
		case 2<<3 | 0: // varint
			v, ni := readVarint(b, i)
			i = ni
			ts = int64(v)
		default:
			t.Fatalf("unexpected Sample field key %d", key)
		}
	}
	return value, ts
}

func TestEncodeWriteRequestRoundTrip(t *testing.T) {
	in := []Series{
		{
			// Deliberately out of order: the encoder must sort them, because
			// Mimir rejects a series whose labels are unsorted.
			Labels: []Label{
				{Name: "namespace", Value: "default"},
				{Name: "__name__", Value: "kubelet_volume_stats_used_bytes"},
				{Name: "cluster", Value: "prod-eu"},
			},
			Value:       153627897856,
			TimestampMS: 1755540000000,
		},
		{
			Labels:      []Label{{Name: "__name__", Value: "peevee_pvc_usage_ratio"}},
			Value:       0.1421,
			TimestampMS: 1755540000000,
		},
	}

	got := decodeWriteRequest(t, EncodeWriteRequest(in))
	if len(got) != 2 {
		t.Fatalf("expected 2 series, got %d", len(got))
	}

	first := got[0]
	if first.labels["__name__"] != "kubelet_volume_stats_used_bytes" {
		t.Errorf("__name__ = %q", first.labels["__name__"])
	}
	if first.labels["cluster"] != "prod-eu" {
		t.Errorf("cluster = %q", first.labels["cluster"])
	}
	if first.value != 153627897856 {
		t.Errorf("value = %v", first.value)
	}
	if first.ts != 1755540000000 {
		t.Errorf("timestamp = %d", first.ts)
	}

	if math.Abs(got[1].value-0.1421) > 1e-9 {
		t.Errorf("fractional value round-tripped as %v", got[1].value)
	}
}

func TestEncodeEmpty(t *testing.T) {
	if b := EncodeWriteRequest(nil); len(b) != 0 {
		t.Errorf("empty request should encode to zero bytes, got %d", len(b))
	}
}

// Varints are the easiest thing to get wrong by hand, so check the boundary
// where the encoding grows a byte.
func TestVarintBoundaries(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 300, 16383, 16384, 1 << 35} {
		b := appendVarint(nil, v)
		got, n := readVarint(b, 0)
		if got != v {
			t.Errorf("varint %d round-tripped as %d", v, got)
		}
		if n != len(b) {
			t.Errorf("varint %d consumed %d of %d bytes", v, n, len(b))
		}
	}
}
