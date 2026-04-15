package usage

import (
	"context"
	"testing"
	"time"
)

type countingPlugin struct {
	ch chan Record
}

func (p *countingPlugin) HandleUsage(_ context.Context, record Record) {
	select {
	case p.ch <- record:
	default:
	}
}

func TestManagerPublishDropsWhenQueueFull(t *testing.T) {
	t.Parallel()

	manager := NewManager(1)
	status := manager.QueueStatus()
	if status.Capacity != 1 {
		t.Fatalf("QueueStatus().Capacity = %d, want 1", status.Capacity)
	}

	manager.Publish(context.Background(), Record{Model: "first"})
	manager.Publish(context.Background(), Record{Model: "second"})

	status = manager.QueueStatus()
	if status.Depth != 1 {
		t.Fatalf("QueueStatus().Depth = %d, want 1", status.Depth)
	}
	if status.Dropped != 1 {
		t.Fatalf("QueueStatus().Dropped = %d, want 1", status.Dropped)
	}
}

func TestManagerDispatchesQueuedRecord(t *testing.T) {
	t.Parallel()

	manager := NewManager(4)
	plugin := &countingPlugin{ch: make(chan Record, 1)}
	manager.Register(plugin)
	manager.Publish(context.Background(), Record{Model: "gpt-5.4"})

	select {
	case got := <-plugin.ch:
		if got.Model != "gpt-5.4" {
			t.Fatalf("record.Model = %q, want %q", got.Model, "gpt-5.4")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatched record")
	}
	manager.Stop()
}
