// embyrelay-db-migrate is an offline database copy tool. It never changes the
// application configuration, removes the source, or copies the master key.
package main

import (
	"context"
	"embyrelay/internal/database"
	"embyrelay/migrations"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migration failed:", err)
		os.Exit(1)
	}
}
func run() error {
	sourceDriver := flag.String("source-driver", "sqlite", "sqlite or postgres")
	targetDriver := flag.String("target-driver", "postgres", "sqlite or postgres")
	source := flag.String("source", "", "SQLite source file")
	target := flag.String("target", "", "SQLite target file")
	sourceFile := flag.String("source-url-file", "", "PostgreSQL source connection file")
	targetFile := flag.String("target-url-file", "", "PostgreSQL target connection file")
	offline := flag.Bool("offline", false, "confirm all writers have been stopped")
	migrationID := flag.String("migration-id", "", "installer identifier for resuming a committed copy")
	flag.Parse()
	if !*offline {
		return fmt.Errorf("stop all writers and pass --offline")
	}
	if *sourceDriver == *targetDriver {
		return fmt.Errorf("source and target database types must differ")
	}
	read := func(driver, path, secret string) (string, error) {
		if driver == "sqlite" {
			if _, err := database.SQLiteFilePath(path); err != nil {
				return "", fmt.Errorf("SQLite requires a file path")
			}
			return path, nil
		}
		if driver != "postgres" || secret == "" {
			return "", fmt.Errorf("PostgreSQL requires a connection file")
		}
		raw, e := os.ReadFile(secret)
		return strings.TrimSpace(string(raw)), e
	}
	fromSource, err := read(*sourceDriver, *source, *sourceFile)
	if err != nil {
		return err
	}
	toSource, err := read(*targetDriver, *target, *targetFile)
	if err != nil {
		return err
	}
	if *sourceDriver == "sqlite" {
		sourcePath, pathErr := database.SQLiteFilePath(fromSource)
		if pathErr != nil {
			return pathErr
		}
		if _, err = os.Stat(sourcePath); err != nil {
			return err
		}
	}
	if *targetDriver == "sqlite" {
		targetPath, pathErr := database.SQLiteFilePath(toSource)
		if pathErr != nil {
			return pathErr
		}
		file, e := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if e != nil {
			return fmt.Errorf("SQLite target must be a new file: %w", e)
		}
		file.Close()
	}
	from, err := database.Open(*sourceDriver, fromSource)
	if err != nil {
		return fmt.Errorf("invalid source configuration")
	}
	defer from.Close()
	to, err := database.Open(*targetDriver, toSource)
	if err != nil {
		return fmt.Errorf("invalid target configuration")
	}
	defer to.Close()
	if err = to.Ping(); err != nil {
		return fmt.Errorf("target database unavailable")
	}
	if to.Driver == "postgres" {
		err = migrations.ApplyPostgres(to.DB)
	} else {
		err = migrations.Apply(to.DB)
	}
	if err != nil {
		return err
	}
	counts, err := database.Copy(context.Background(), from, to, *migrationID)
	if err != nil {
		return err
	}
	var total int64
	for _, count := range counts {
		total += count
	}
	fmt.Printf("Copied %d rows. Source preserved. Keep the original master key when switching databases.\n", total)
	return nil
}
