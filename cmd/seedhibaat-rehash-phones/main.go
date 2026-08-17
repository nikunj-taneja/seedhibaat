// One-off repair: bring customers.phone_hash onto the canonical phone form.
//
// Shopify order addresses hold local numbers with no country code, while Meta
// reports full international digits. Rows stored before the canonical form
// existed hash differently, so the same person has two identities and cannot
// be found by phone.
//
// Dry run by default. Prints counts only, never PII.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/nikunj-taneja/seedhibaat/internal/security"
	_ "modernc.org/sqlite"
)

func canonical(value, defaultCountry string) string {
	var builder strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			builder.WriteRune(r)
		}
	}
	digits := strings.TrimPrefix(builder.String(), "00")
	country := strings.TrimPrefix(strings.TrimSpace(defaultCountry), "+")
	if country != "" && len(digits) > 0 && len(digits) <= 10 && !strings.HasPrefix(digits, country) {
		digits = country + digits
	}
	return digits
}

func main() {
	apply := flag.Bool("apply", false, "write the canonical hashes instead of reporting them")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Println("usage: rehash-phones [--apply] <database>")
		os.Exit(2)
	}
	key := os.Getenv("SEEDHIBAAT_PII_HASH_KEY")
	country := os.Getenv("SEEDHIBAAT_DEFAULT_COUNTRY_CODE")
	if key == "" || country == "" {
		fmt.Println("SEEDHIBAAT_PII_HASH_KEY and SEEDHIBAAT_DEFAULT_COUNTRY_CODE are required")
		os.Exit(1)
	}
	db, err := sql.Open("sqlite", flag.Arg(0)+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	ctx := context.Background()

	rows, err := db.QueryContext(ctx, `SELECT id, phone_ciphertext, phone_hash FROM customers WHERE phone_ciphertext IS NOT NULL`)
	if err != nil {
		panic(err)
	}
	type change struct {
		id         int64
		ciphertext []byte
		hash       string
	}
	var changes []change
	unchanged, failed := 0, 0
	for rows.Next() {
		var id int64
		var ciphertext []byte
		var hash string
		if err := rows.Scan(&id, &ciphertext, &hash); err != nil {
			panic(err)
		}
		plain, err := security.Decrypt(key, ciphertext)
		if err != nil {
			failed++
			continue
		}
		canonicalPhone := canonical(string(plain), country)
		canonicalHash := security.KeyedHash(key, canonicalPhone)
		if canonicalHash == hash {
			unchanged++
			continue
		}
		encrypted, err := security.Encrypt(key, []byte(canonicalPhone))
		if err != nil {
			failed++
			continue
		}
		changes = append(changes, change{id: id, ciphertext: encrypted, hash: canonicalHash})
	}
	rows.Close()

	// Two rows canonicalising to one number are the same person recorded
	// twice. Merging them is not this tool's job, so report and skip.
	seen := map[string]int{}
	for _, c := range changes {
		seen[c.hash]++
	}
	collisions := 0
	for _, count := range seen {
		if count > 1 {
			collisions += count
		}
	}
	fmt.Printf("already canonical: %d\nto rewrite:        %d\nundecryptable:     %d\ncollisions:        %d\n", unchanged, len(changes), failed, collisions)
	if !*apply {
		fmt.Println("\nDry run. Re-run with --apply to write.")
		return
	}
	if collisions > 0 {
		fmt.Println("Refusing to write while two customers share a canonical number.")
		os.Exit(1)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		panic(err)
	}
	defer tx.Rollback()
	written := 0
	for _, c := range changes {
		var clash int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM customers WHERE phone_hash=? AND id<>?`, c.hash, c.id).Scan(&clash); err != nil {
			panic(err)
		}
		if clash > 0 {
			fmt.Printf("skipped customer %d: another row already holds that number\n", c.id)
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE customers SET phone_ciphertext=?,phone_hash=?,updated_at=datetime('now') WHERE id=?`, c.ciphertext, c.hash, c.id); err != nil {
			panic(err)
		}
		written++
	}
	if err := tx.Commit(); err != nil {
		panic(err)
	}
	fmt.Printf("rewritten: %d\n", written)
}
