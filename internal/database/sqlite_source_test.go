package database

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteEscapesFileNamesAndPreservesURIOptions(t *testing.T) {
	for _, uri := range []bool{false, true} {
		t.Run(map[bool]string{false: "path", true: "uri"}[uri], func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "data?#% space")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "db?#%.sqlite")
			source := path
			if uri {
				source = (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=rwc&_pragma=cache_size%28123%29"}).String()
			}
			resolved, err := SQLiteFilePath(source)
			if err != nil || resolved != path {
				t.Fatal("URI filesystem path", resolved, err)
			}
			db, err := Open("sqlite", source)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err = db.Exec("CREATE TABLE parent(id INTEGER PRIMARY KEY);CREATE TABLE child(id INTEGER REFERENCES parent(id));INSERT INTO parent VALUES(1)"); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("INSERT INTO child VALUES(2)"); err == nil {
				t.Fatal("foreign keys disabled")
			}
			if _, err = os.Stat(path); err != nil {
				t.Fatal("wrong database filename", err)
			}
			for i := 0; i < 2; i++ {
				if i == 1 {
					db.SetMaxIdleConns(0)
				}
				var fk, timeout int
				if err = db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
					t.Fatal(err)
				}
				if err = db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
					t.Fatal(err)
				}
				if fk != 1 || timeout != 5000 {
					t.Fatal("connection pragma lost", fk, timeout)
				}
				if uri {
					var cache int
					if err = db.QueryRow("PRAGMA cache_size").Scan(&cache); err != nil || cache != 123 {
						t.Fatal("existing URI parameters lost", cache, err)
					}
				}
			}
		})
	}
}
