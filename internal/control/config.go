package control

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
)

func LoadMaster(dir, encoded string) ([]byte, error) {
	if encoded != "" {
		b, e := base64.RawStdEncoding.DecodeString(encoded)
		if e != nil || len(b) != 32 {
			return nil, fmt.Errorf("RELAY_MASTER_KEY must be 32 bytes in unpadded base64")
		}
		return b, nil
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	path := filepath.Join(dir, "master.key")
	b, e := os.ReadFile(path)
	if e == nil {
		if len(b) != 32 {
			return nil, fmt.Errorf("invalid master.key")
		}
		return b, nil
	}
	if !os.IsNotExist(e) {
		return nil, e
	}
	b = make([]byte, 32)
	if _, e = rand.Read(b); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	if _, e = f.Write(b); e != nil {
		return nil, e
	}
	if e = f.Sync(); e != nil {
		return nil, e
	}
	return b, nil
}
