// Read-only diagnostic: does customers.phone_hash match a digits-only
// normalisation of the stored phone? Prints counts only, never PII.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/nikunj-taneja/seedhibaat/internal/security"
	_ "modernc.org/sqlite"
)

func digitsOnly(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func main() {
	key := os.Getenv("SEEDHIBAAT_PII_HASH_KEY")
	if key == "" {
		fmt.Println("missing SEEDHIBAAT_PII_HASH_KEY")
		os.Exit(1)
	}
	db, err := sql.Open("sqlite", os.Args[1]+"?_pragma=query_only(1)&mode=ro")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), `SELECT id, phone_ciphertext, phone_hash, shopify_id IS NOT NULL, created_at FROM customers WHERE phone_ciphertext IS NOT NULL`)
	if err != nil {
		panic(err)
	}
	defer rows.Close()

	var total, decryptFail, hasNonDigit, hashMatchesStored int
	byDigits := map[string][]int64{}
	withShopify := map[string]bool{}
	created := map[string]int{}
	for rows.Next() {
		var id int64
		var ciphertext []byte
		var hash string
		var hasShopify bool
		var createdAt string
		if err := rows.Scan(&id, &ciphertext, &hash, &hasShopify, &createdAt); err != nil {
			panic(err)
		}
		total++
		plain, err := security.Decrypt(key, ciphertext)
		if err != nil {
			decryptFail++
			continue
		}
		stored := strings.TrimPrefix(strings.TrimSpace(string(plain)), "+")
		if stored != digitsOnly(stored) {
			hasNonDigit++
		}
		if security.KeyedHash(key, stored) == hash {
			hashMatchesStored++
		}
		d := digitsOnly(stored)
		byDigits[d] = append(byDigits[d], id)
		if hasShopify {
			withShopify[d] = true
		}
		if strings.HasPrefix(createdAt, "2026-08-17") {
			created[d]++
		}
	}

	duplicateGroups, duplicateRows, repairable := 0, 0, 0
	for d, ids := range byDigits {
		if len(ids) > 1 {
			duplicateGroups++
			duplicateRows += len(ids)
			if withShopify[d] && created[d] > 0 {
				repairable++
			}
		}
	}
	fmt.Printf("customers with a phone:            %d\n", total)
	fmt.Printf("  undecryptable:                   %d\n", decryptFail)
	fmt.Printf("  stored phone has non-digits:     %d\n", hasNonDigit)
	fmt.Printf("  phone_hash matches stored form:  %d\n", hashMatchesStored)
	fmt.Printf("duplicate identities (same digits): %d groups, %d rows\n", duplicateGroups, duplicateRows)
	fmt.Printf("  groups pairing a Shopify customer with a row created today: %d\n", repairable)
}
