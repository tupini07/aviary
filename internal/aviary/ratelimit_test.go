package aviary

import (
	"testing"
	"time"
)

func TestLoginRateLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	rl := newLoginRateLimiter(3, time.Minute)
	rl.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if ok, _ := rl.allow("1.2.3.4"); !ok {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	ok, retry := rl.allow("1.2.3.4")
	if ok {
		t.Fatal("4th attempt should be blocked")
	}
	if retry <= 0 {
		t.Fatalf("retry duration = %v, want > 0", retry)
	}

	// A different IP is independent.
	if ok, _ := rl.allow("5.6.7.8"); !ok {
		t.Fatal("different IP should be allowed")
	}

	// After the window elapses, the original IP is allowed again.
	now = now.Add(2 * time.Minute)
	if ok, _ := rl.allow("1.2.3.4"); !ok {
		t.Fatal("attempt after window reset should be allowed")
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		remote string
		xff    string
		want   string
	}{
		{"1.2.3.4:5678", "", "1.2.3.4"},
		{"1.2.3.4:5678", "9.9.9.9, 1.2.3.4", "9.9.9.9"},
		{"1.2.3.4:5678", "  8.8.8.8  ", "8.8.8.8"},
	}
	for _, c := range cases {
		if got := clientIPFrom(c.remote, c.xff); got != c.want {
			t.Errorf("clientIP(remote=%q, xff=%q) = %q, want %q", c.remote, c.xff, got, c.want)
		}
	}
}
