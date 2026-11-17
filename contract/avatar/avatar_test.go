package avatar

import (
	"bytes"
	"image/png"
	"strings"
	"testing"
)

func TestPNGDeterministicAndValid(t *testing.T) {
	pk := strings.Repeat("ab", 32)
	a, err := PNG(pk)
	if err != nil {
		t.Fatal(err)
	}
	b, err := PNG(pk)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("the same pubkey must render the same avatar")
	}
	img, err := png.Decode(bytes.NewReader(a))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	if img.Bounds().Dx() != Size || img.Bounds().Dy() != Size {
		t.Fatalf("size = %v, want %dx%d", img.Bounds(), Size, Size)
	}
	other, _ := PNG(strings.Repeat("cd", 32))
	if bytes.Equal(a, other) {
		t.Fatal("different pubkeys must render different avatars")
	}
}

func TestPNGRejectsBadPubkey(t *testing.T) {
	for _, bad := range []string{"", "abc", strings.Repeat("z", 64)} {
		if _, err := PNG(bad); err == nil {
			t.Errorf("pubkey %q accepted", bad)
		}
	}
}

func TestURL(t *testing.T) {
	pk := strings.Repeat("ab", 32)
	if got, want := URL("https://cp.example/", pk), "https://cp.example/avatars/"+pk+".png"; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
	if URL("", pk) != "" {
		t.Fatal("empty host must not render a URL")
	}
}
