package tasks

import (
	"reflect"
	"testing"
	"time"
)

func TestAwaiting(t *testing.T) {
	tests := []struct {
		name, status, last, want string
	}{
		{"open, brain spoke last", StatusOpen, TypeBrainReply, AwaitingOperator},
		{"open, operator spoke last", StatusOpen, TypeChatMessage, AwaitingFactory},
		{"open, no visible message", StatusOpen, "", AwaitingFactory},
		{"closed, brain spoke last", StatusClosed, TypeBrainReply, AwaitingFactory},
		{"closed, operator spoke last", StatusClosed, TypeChatMessage, AwaitingFactory},
		// Internal types are never passed in by the server (the query
		// filters them), but must never count as the brain speaking.
		{"internal type is not a brain reply", StatusOpen, "foreman.report", AwaitingFactory},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Awaiting(tt.status, tt.last); got != tt.want {
				t.Errorf("Awaiting(%q,%q) = %q, want %q", tt.status, tt.last, got, tt.want)
			}
		})
	}
}

func TestOrderWaiting(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 11, 16, 10, m, 0, 0, time.UTC) }
	items := []WaitingItem{
		{"c", 2, at(1)}, {"a", 0, at(30)}, {"b", 0, at(5)}, {"d", 1, at(0)}, {"e", 0, at(5)},
	}
	OrderWaiting(items)
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	// P0 first (oldest waiting first, id breaks the tie), then P1, then P2.
	if want := []string{"b", "e", "a", "d", "c"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("order = %v, want %v", ids, want)
	}
}

func TestMergeQueue(t *testing.T) {
	tests := []struct {
		name        string
		prev, fresh []string
		want        []string
	}{
		{"empty start takes fresh order", nil, []string{"a", "b"}, []string{"a", "b"}},
		{"new urgent task joins the end, does not displace", []string{"b", "c"}, []string{"a", "b", "c"}, []string{"b", "c", "a"}},
		{"answered task drops out", []string{"a", "b", "c"}, []string{"a", "c"}, []string{"a", "c"}},
		{"nothing waiting", []string{"a"}, nil, []string{}},
		{"duplicates collapse", []string{"a", "a"}, []string{"a"}, []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MergeQueue(tt.prev, tt.fresh); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MergeQueue = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAdvance(t *testing.T) {
	tests := []struct {
		name    string
		queue   []string
		current string
		want    string
	}{
		{"next in queue", []string{"a", "b", "c"}, "a", "b"},
		{"middle", []string{"a", "b", "c"}, "b", "c"},
		{"wraps to first", []string{"a", "b", "c"}, "c", "a"},
		{"only item: nothing else", []string{"a"}, "a", ""},
		{"current already dropped: head", []string{"b", "c"}, "a", "b"},
		{"empty queue", nil, "a", ""},
		{"no current: head", []string{"a", "b"}, "", "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Advance(tt.queue, tt.current); got != tt.want {
				t.Errorf("Advance(%v,%q) = %q, want %q", tt.queue, tt.current, got, tt.want)
			}
		})
	}
}
