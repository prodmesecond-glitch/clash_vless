package engine

import "testing"

func TestMeanSpeed(t *testing.T) {
	s := &Supervisor{}
	if mbps, n := s.meanSpeed("x"); mbps != 0 || n != 0 {
		t.Fatalf("meanSpeed(unknown) = %g over %d, want 0 over 0", mbps, n)
	}
	s.recordSpeed("x", 40)
	s.recordSpeed("x", 60)
	if mbps, n := s.meanSpeed("x"); mbps != 50 || n != 2 {
		t.Fatalf("meanSpeed(x) = %g over %d, want 50 over 2", mbps, n)
	}
}

func TestFlakinessAndStabilityBucket(t *testing.T) {
	s := &Supervisor{}

	// 7 pass + 3 fail within the window => 30% flaky over 10 samples.
	for i := 0; i < 7; i++ {
		s.recordProbe("n", true)
	}
	for i := 0; i < 3; i++ {
		s.recordProbe("n", false)
	}
	if pct, n := s.flakiness("n"); pct != 30 || n != 10 {
		t.Fatalf("flakiness(n) = %g%% over %d, want 30%% over 10", pct, n)
	}
	if b := s.stabilityBucket("n"); b != 3 { // 30 / 10
		t.Fatalf("stabilityBucket(n) = %d, want 3", b)
	}

	// Under-sampled (1 sample, all-fail) must NOT sink to the worst band — it lands
	// mid-pack via unknownFlakePct so a proven-steady node beats it but it beats a
	// proven-flaky one.
	s.recordProbe("m", false)
	if b := s.stabilityBucket("m"); b != 2 { // unknownFlakePct(25) / 10
		t.Fatalf("stabilityBucket(m, under-sampled) = %d, want 2", b)
	}

	// A node with no history at all is also mid-pack, not band 0.
	if pct, n := s.flakiness("none"); pct != 0 || n != 0 {
		t.Fatalf("flakiness(none) = %g%% over %d, want 0%% over 0", pct, n)
	}
	if b := s.stabilityBucket("none"); b != 2 {
		t.Fatalf("stabilityBucket(none) = %d, want 2", b)
	}

	// A steady node (0% over enough samples) sits in band 0, ahead of everyone.
	for i := 0; i < minStabSamples; i++ {
		s.recordProbe("solid", true)
	}
	if b := s.stabilityBucket("solid"); b != 0 {
		t.Fatalf("stabilityBucket(solid) = %d, want 0", b)
	}
}
