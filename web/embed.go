package web

import (
	"embed"
	"io/fs"
)

//go:embed dist/*
var files embed.FS

func Assets() fs.FS {
	f, e := fs.Sub(files, "dist")
	if e != nil {
		panic(e)
	}
	return f
}
