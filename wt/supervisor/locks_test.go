package main

import (
	"testing"
	"time"
)

func TestLockAppIsPerApp(t *testing.T) {
	s := testSupervisor()
	unlockA := s.lockApp("a")

	done := make(chan struct{})

	go func() {
		unlockB := s.lockApp("b")

		close(done)
		unlockB()
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("app b was blocked by app a")
	}

	unlockA()
}

func TestLockAppSerializesSameApp(t *testing.T) {
	s := testSupervisor()
	unlockFirst := s.lockApp("a")

	done := make(chan struct{})

	go func() {
		unlockSecond := s.lockApp("a")

		close(done)
		unlockSecond()
	}()

	select {
	case <-done:
		t.Fatal("the same app lock was acquired twice")
	case <-time.After(50 * time.Millisecond):
	}

	unlockFirst()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the same app lock did not wake after unlock")
	}
}
