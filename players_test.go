package main

import "testing"

func TestPlayerSlotsReuseLowest(t *testing.T) {
	a := addPlayer("10.0.0.1:1000")
	b := addPlayer("10.0.0.2:1000")
	c := addPlayer("10.0.0.3:1000")
	if a.Slot != 1 || b.Slot != 2 || c.Slot != 3 {
		t.Fatalf("slots %d %d %d, want 1 2 3", a.Slot, b.Slot, c.Slot)
	}
	removePlayer(a)
	d := addPlayer("10.0.0.4:1000")
	if d.Slot != 1 {
		t.Errorf("after freeing slot 1, got slot %d", d.Slot)
	}
	removePlayer(b)
	removePlayer(c)
	removePlayer(d)
}
