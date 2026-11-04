package secrets

import (
	"fmt"
	"os"
	"testing"
)

// TestPrintFixture prints a sealed secret for tests/secrets_fixture.json (run
// by hand with SEAL_PUBLIC_KEY set; skipped otherwise).
func TestPrintFixture(t *testing.T) {
	pub := os.Getenv("SEAL_PUBLIC_KEY")
	if pub == "" {
		t.Skip("SEAL_PUBLIC_KEY not set")
	}
	k, err := PublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Seal(k, "core_banking", "Bearer s3cret-token")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("SEALED=" + s)
}
