package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUIDAllocatorStableAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".uids.json")

	first, err := newUIDAllocator(path, 20000)
	if err != nil {
		t.Fatal(err)
	}

	alpha, err := first.Allocate("alpha")
	if err != nil {
		t.Fatal(err)
	}

	beta, err := first.Allocate("beta")
	if err != nil {
		t.Fatal(err)
	}

	if alpha != 20000 || beta != 20001 {
		t.Fatalf("uids = %d, %d, want 20000, 20001", alpha, beta)
	}

	if again, err := first.Allocate("alpha"); err != nil || again != alpha {
		t.Fatalf("second allocation of alpha = %d, %v", again, err)
	}

	reloaded, err := newUIDAllocator(path, 20000)
	if err != nil {
		t.Fatal(err)
	}

	if uid, ok := reloaded.UID("alpha"); !ok || uid != alpha {
		t.Errorf("alpha after reload = %d, %v, want %d", uid, ok, alpha)
	}

	if uid, ok := reloaded.UID("beta"); !ok || uid != beta {
		t.Errorf("beta after reload = %d, %v, want %d", uid, ok, beta)
	}

	gamma, err := reloaded.Allocate("gamma")
	if err != nil {
		t.Fatal(err)
	}

	if gamma != 20002 {
		t.Errorf("gamma = %d, want 20002", gamma)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Errorf("uid file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestUIDAllocatorResolvesCollisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".uids.json")

	data := `{"apps":{"beta":20000,"alpha":20000,"below":100}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	allocator, err := newUIDAllocator(path, 20000)
	if err != nil {
		t.Fatal(err)
	}

	alpha, ok := allocator.UID("alpha")
	if !ok {
		t.Fatal("alpha was dropped")
	}

	beta, ok := allocator.UID("beta")
	if !ok {
		t.Fatal("beta was dropped")
	}

	if alpha == beta {
		t.Errorf("collision not resolved: alpha and beta both %d", alpha)
	}

	if alpha != 20000 {
		t.Errorf("sorted first claimant alpha = %d, want 20000", alpha)
	}

	if beta != 20001 {
		t.Errorf("reassigned beta = %d, want 20001", beta)
	}

	if _, ok := allocator.UID("below"); ok {
		t.Error("a uid below the base should have been dropped")
	}

	gamma, err := allocator.Allocate("gamma")
	if err != nil {
		t.Fatal(err)
	}

	if gamma != 20002 {
		t.Errorf("gamma = %d, want 20002", gamma)
	}
}

func TestUIDAllocatorRejectsBadBase(t *testing.T) {
	if _, err := newUIDAllocator(filepath.Join(t.TempDir(), ".uids.json"), 0); err == nil {
		t.Error("base 0 should be rejected")
	}
}
