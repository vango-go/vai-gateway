package upstream

import "testing"

func TestFactoryNew_GemDev(t *testing.T) {
	f := Factory{}
	p, err := f.New("gem-dev", "test-key")
	if err != nil {
		t.Fatalf("New returned err=%v", err)
	}
	if p == nil {
		t.Fatal("provider is nil")
	}
}

func TestFactoryNew_GemVert(t *testing.T) {
	f := Factory{}
	p, err := f.New("gem-vert", "test-key")
	if err != nil {
		t.Fatalf("New returned err=%v", err)
	}
	if p == nil {
		t.Fatal("provider is nil")
	}
}
