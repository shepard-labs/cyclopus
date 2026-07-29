package auth

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPasswordHashAndVerify(t *testing.T) {
	params := PasswordParams{MemoryKiB: minMemoryKiB, Iterations: 1, Parallelism: 1, SaltBytes: 16}
	hash, err := HashPassword("correct horse battery staple", params)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, "correct horse battery staple") {
		t.Fatal("encoded hash contains plaintext password")
	}
	valid, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil || !valid {
		t.Fatalf("VerifyPassword(correct) = %v, %v", valid, err)
	}
	valid, err = VerifyPassword("wrong", hash)
	if err != nil || valid {
		t.Fatalf("VerifyPassword(wrong) = %v, %v", valid, err)
	}
}

func TestPasswordHashConcurrencyCap(t *testing.T) {
	if err := SetMaxConcurrentHashes(2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetMaxConcurrentHashes(4) })

	release := make(chan struct{})
	started := make(chan struct{}, 4)
	var active atomic.Int32
	var peak atomic.Int32
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			withPasswordHashSlot(func() []byte {
				current := active.Add(1)
				for observed := peak.Load(); current > observed && !peak.CompareAndSwap(observed, current); observed = peak.Load() {
				}
				started <- struct{}{}
				<-release
				active.Add(-1)
				return nil
			})
		}()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("password hash slot did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("more password hash work started than the configured cap")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	if peak.Load() != 2 {
		t.Fatalf("peak concurrent password hashes = %d, want 2", peak.Load())
	}
}

func TestPasswordVerificationRejectsUnboundedOrMalformedHashes(t *testing.T) {
	for _, encoded := range []string{
		"",
		"$argon2id$v=19$m=1048576,t=3,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=65536,t=99,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=65536,t=3,p=255$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2i$v=19$m=65536,t=3,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	} {
		if valid, err := VerifyPassword("password", encoded); valid || !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("VerifyPassword(%q) = %v, %v", encoded, valid, err)
		}
	}
}
