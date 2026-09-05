// Measures the byte size of what the client folds into the Live handshake, straight from the store.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ora/internal/db"
)

func main() {
	home, _ := os.UserHomeDir()
	store, err := db.New(filepath.Join(home, ".local/share/ora/db"))
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	defer store.Close()
	ctx := context.Background()
	impl, err := store.GetImplicitContext(ctx)
	fmt.Println("implicit context lines:", len(impl), "bytes:", len(strings.Join(impl, "\n")), "err:", err)
	pc, err := store.PersonalContext(ctx)
	n := 0
	for _, e := range pc {
		n += len(e.Subject) + len(e.Content)
	}
	fmt.Println("personal context entries:", len(pc), "bytes:", n, "err:", err)
}
