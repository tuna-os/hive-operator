package controller

import "testing"

// TestB2F pins the gauge encoding: metrics consumers alert on == 0, so an
// accidental inversion here would silently invert every alert.
func TestB2F(t *testing.T) {
	if b2f(true) != 1 || b2f(false) != 0 {
		t.Fatalf("b2f encoding changed: true=%v false=%v", b2f(true), b2f(false))
	}
}
