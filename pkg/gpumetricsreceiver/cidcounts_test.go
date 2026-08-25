package gpumetricsreceiver

import (
	"sync"
	"testing"

	"go.mws.cloud/gpu-metrics-exporter/pkg/vsock/common"
	"go.uber.org/zap"
)

// TestServeConnectionReleasesCIDSlot covers the increment/decrement symmetry of
// the per-CID limiter. serveConnection increments unconditionally, so teardown
// must decrement unconditionally too — under the CID captured at accept time,
// not one re-read from the kernel at teardown. A guard that skips the release
// when the re-read fails would burn one of the VM's slots per connection, and
// since the exporter opens a fresh connection every tick, the VM would hit
// MaxConnectionsPerCID and be locked out for the life of the receiver.
func TestServeConnectionReleasesCIDSlot(t *testing.T) {
	sender, receiverConn := socketpairConns(t)

	r := &GpuMetricsReceiver{
		log:                  zap.NewNop(),
		metricsConsumer:      &recordingConsumer{},
		connections:          make(map[*common.VsockConn]struct{}),
		cidCounts:            make(map[uint32]int),
		maxConnections:       64,
		maxConnectionsPerCID: 4,
	}

	// Close the sender so handleConnection sees EOF and runs its teardown.
	_ = sender.Close()

	r.connections[receiverConn] = struct{}{}
	cid := receiverConn.RemoteAddr().CID
	r.cidCounts[cid] = 1

	r.wg.Add(1)
	r.handleConnection(receiverConn)
	r.wg.Wait()

	if n, ok := r.cidCounts[cid]; ok {
		t.Fatalf("cidCounts[%d] = %d after teardown, want the entry removed; "+
			"the VM permanently loses a connection slot", cid, n)
	}
	if len(r.connections) != 0 {
		t.Fatalf("connections not drained: %d left", len(r.connections))
	}
}

// TestShutdownClearsCIDCounts covers the other half. shutdown() drains
// r.connections, and handleConnection only releases a slot when it still finds
// its conn there — so those releases never run and shutdown must clear the
// counts itself. Run() resets `stopping` and rebuilds the listener, so a
// receiver can be re-run; stale counts would carry into the next Run and reject
// the affected CIDs immediately.
func TestShutdownClearsCIDCounts(t *testing.T) {
	r := &GpuMetricsReceiver{
		log:           zap.NewNop(),
		connections:   make(map[*common.VsockConn]struct{}),
		cidCounts:     map[uint32]int{3: 2, 4: 1},
		connectionsMu: sync.Mutex{},
	}

	r.shutdown()

	if len(r.cidCounts) != 0 {
		t.Fatalf("cidCounts = %v after shutdown, want empty; a re-Run would start "+
			"these CIDs partway to their per-CID cap", r.cidCounts)
	}
}
