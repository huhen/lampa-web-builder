package builder

import "testing"

func TestQueuePushPopOrder(t *testing.T) {
	q := newQueue(3)
	for _, d := range []string{"a", "b", "c"} {
		if !q.Push(d) {
			t.Fatalf("push %q failed", d)
		}
	}
	for _, want := range []string{"a", "b", "c"} {
		got, ok := q.Pop()
		if !ok || got != want {
			t.Fatalf("pop = %q,%v want %q", got, ok, want)
		}
	}
	if _, ok := q.Pop(); ok {
		t.Error("empty queue popped")
	}
}

func TestQueueCapacity(t *testing.T) {
	q := newQueue(2)
	if !q.Push("a") || !q.Push("b") {
		t.Fatal("initial pushes failed")
	}
	if q.Push("c") {
		t.Error("push beyond capacity must fail")
	}
	if q.Len() != 2 {
		t.Errorf("len = %d, want 2", q.Len())
	}
}

func TestQueueRemove(t *testing.T) {
	q := newQueue(3)
	if !q.Push("a") {
		t.Fatal("push a failed")
	}
	if !q.Push("b") {
		t.Fatal("push b failed")
	}
	if !q.Push("c") {
		t.Fatal("push c failed")
	}
	if !q.Remove("b") {
		t.Error("remove b failed")
	}
	if q.Remove("b") {
		t.Error("second remove b must fail")
	}
	if got := q.Items(); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("items = %v, want [a c]", got)
	}
}

func TestQueueItemsIsCopy(t *testing.T) {
	q := newQueue(2)
	q.Push("a")
	items := q.Items()
	items[0] = "mutated"
	if got := q.Items(); got[0] != "a" {
		t.Errorf("Items() leaked internal slice: %v", got)
	}
}
