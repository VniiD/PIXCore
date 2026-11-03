package secureio_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pixcore/pixcore/libs/platform/secureio"
)

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "s")
	if err := os.WriteFile(ok, []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := secureio.ReadFile(ok); err != nil || string(b) != "v" {
		t.Fatalf("%q %v", b, err)
	}
	// symlink interno ao diretório (padrão de secrets Kubernetes) é aceito
	link := filepath.Join(dir, "link")
	if err := os.Symlink("s", link); err != nil {
		t.Fatal(err)
	}
	if _, err := secureio.ReadFile(link); err != nil {
		t.Fatalf("internal symlink: %v", err)
	}
	// symlink que escapa do diretório é recusado
	esc := filepath.Join(dir, "esc")
	if err := os.Symlink("/etc/passwd", esc); err != nil {
		t.Fatal(err)
	}
	if _, err := secureio.ReadFile(esc); err == nil {
		t.Fatal("escaping symlink must fail")
	}
	for _, p := range []string{"relative", dir + "/../" + filepath.Base(dir) + "/s", dir, dir + "//s"} {
		if _, err := secureio.ReadFile(p); err == nil {
			t.Fatalf("%q must fail", p)
		}
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", secureio.MaxConfigSize+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := secureio.ReadFile(big); err == nil {
		t.Fatal("oversized file must fail")
	}
}
