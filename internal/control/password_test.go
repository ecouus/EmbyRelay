package control

import (
	"golang.org/x/crypto/bcrypt"
	"strings"
	"testing"
)

func TestPasswordLengthBoundaries(t *testing.T) {
	for _, n := range []int{7, 8, 72, 73} {
		t.Run(strings.Repeat("x", n), func(t *testing.T) {
			value := strings.Repeat("x", n)
			hash, err := password(value)
			if n < 8 || n > 72 {
				if err == nil {
					t.Fatal("expected invalid length to be rejected")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(value)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
