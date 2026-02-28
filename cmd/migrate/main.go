package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "github.com/lib/pq"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}

	dir := "migrations"
	listOnly := false
	for _, a := range os.Args[1:] {
		if a == "--list" {
			listOnly = true
		} else {
			dir = a
		}
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("ping: %v", err)
	}
	log.Println("Connected to database")

	if listOnly {
		rows, err := db.Query("SELECT tablename FROM pg_tables WHERE schemaname='public' AND tablename LIKE 'mailing_%' ORDER BY tablename")
		if err != nil {
			log.Fatal(err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var t string
			rows.Scan(&t)
			fmt.Println(" ", t)
			n++
		}
		fmt.Printf("Total: %d tables\n", n)
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Fatalf("read migrations dir %s: %v", dir, err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	var okCount, errCount int
	for _, f := range files {
		path := filepath.Join(dir, f)
		data, err := os.ReadFile(path)
		if err != nil {
			log.Fatalf("read %s: %v", path, err)
		}
		content := string(data)
		if strings.TrimSpace(content) == "" {
			continue
		}
		fmt.Printf("  %s ... ", f)

		tx, err := db.Begin()
		if err != nil {
			fmt.Printf("BEGIN ERROR: %v\n", err)
			errCount++
			continue
		}
		if _, err := tx.Exec(content); err != nil {
			tx.Rollback()
			fmt.Printf("ERROR: %v\n", err)
			errCount++
		} else {
			tx.Commit()
			fmt.Println("OK")
			okCount++
		}
	}
	log.Printf("Done: %d OK, %d errors", okCount, errCount)
	log.Println("Migrations complete")
}
