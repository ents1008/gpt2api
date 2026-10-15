package chatgpt

import (
	"io"
	"net"
	"testing"
	"time"
)

type memoryConn struct {
	reads  []byte
	writes []byte
}

func (conn *memoryConn) Read(buffer []byte) (int, error) {
	if len(conn.reads) == 0 {
		return 0, io.EOF
	}
	count := copy(buffer, conn.reads)
	conn.reads = conn.reads[count:]
	return count, nil
}

func (conn *memoryConn) Write(buffer []byte) (int, error) {
	conn.writes = append(conn.writes, buffer...)
	return len(buffer), nil
}

func (conn *memoryConn) Close() error                       { return nil }
func (conn *memoryConn) LocalAddr() net.Addr                { return nil }
func (conn *memoryConn) RemoteAddr() net.Addr               { return nil }
func (conn *memoryConn) SetDeadline(_ time.Time) error      { return nil }
func (conn *memoryConn) SetReadDeadline(_ time.Time) error  { return nil }
func (conn *memoryConn) SetWriteDeadline(_ time.Time) error { return nil }

// Purpose: pin paid-proxy accounting to the actual proxy socket. Expected:
// every successful byte read from or written to the wrapped connection is
// accumulated exactly once, while EOF contributes zero bytes.
func TestMeteredConnCountsProxySocketBytes(t *testing.T) {
	meter := &TrafficMeter{}
	base := &memoryConn{reads: []byte("proxy-response")}
	conn := &meteredConn{Conn: base, meter: meter}
	buffer := make([]byte, 64)
	read, err := conn.Read(buffer)
	if err != nil || string(buffer[:read]) != "proxy-response" {
		t.Fatalf("Read() = (%q, %v)", buffer[:read], err)
	}
	if _, err := conn.Read(buffer); err != io.EOF {
		t.Fatalf("second Read() error = %v, want EOF", err)
	}
	if written, err := conn.Write([]byte("proxy-request")); err != nil || written != 13 {
		t.Fatalf("Write() = (%d, %v)", written, err)
	}

	snapshot := meter.Snapshot()
	if snapshot.ProxyReadBytes != 14 || snapshot.ProxyWrittenBytes != 13 {
		t.Fatalf("traffic snapshot = %+v", snapshot)
	}
}

// Purpose: keep nested requirements measurements ordered and immutable after
// export. Expected: each checkpoint preserves the cumulative socket counters
// at that exact stage, and mutating a returned slice cannot corrupt the meter.
func TestTrafficMeterCheckpointsPreserveOrderedSnapshots(t *testing.T) {
	meter := &TrafficMeter{}
	meter.proxyReadBytes.Store(10)
	meter.proxyWrittenBytes.Store(4)
	meter.checkpoint("requirements.bootstrap")
	meter.proxyReadBytes.Add(30)
	meter.proxyWrittenBytes.Add(7)
	meter.checkpoint("requirements.prepare")

	checkpoints := meter.Checkpoints()
	if len(checkpoints) != 2 ||
		checkpoints[0].Stage != "requirements.bootstrap" ||
		checkpoints[0].Traffic.ProxyReadBytes != 10 ||
		checkpoints[0].Traffic.ProxyWrittenBytes != 4 ||
		checkpoints[1].Stage != "requirements.prepare" ||
		checkpoints[1].Traffic.ProxyReadBytes != 40 ||
		checkpoints[1].Traffic.ProxyWrittenBytes != 11 {
		t.Fatalf("traffic checkpoints = %+v", checkpoints)
	}
	checkpoints[0].Stage = "mutated"
	if got := meter.Checkpoints()[0].Stage; got != "requirements.bootstrap" {
		t.Fatalf("stored checkpoint mutated through exported slice: %q", got)
	}
}
